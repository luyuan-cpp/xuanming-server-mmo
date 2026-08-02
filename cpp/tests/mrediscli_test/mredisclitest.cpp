#include <gtest/gtest.h>
#include <string>

#include "contrib/hiredis/Hiredis.h"
#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"
#include "c2gw.pb.h"

using namespace muduo;
using namespace muduo::net;

using MessageCachedArray = std::vector<uint8_t>;

string toString(long long value)
{
    char buf[32];
    snprintf(buf, sizeof buf, "%lld", value);
    return buf;
}

string redisReplyToString(const redisReply* reply)
{
    static const char* const types[] = { "",
        "REDIS_REPLY_STRING", "REDIS_REPLY_ARRAY",
        "REDIS_REPLY_INTEGER", "REDIS_REPLY_NIL",
        "REDIS_REPLY_STATUS", "REDIS_REPLY_ERROR" };
    string str;
    if (!reply) return str;

    str += types[reply->type] + string("(") + toString(reply->type) + ") ";

    str += "{ ";
    if (reply->type == REDIS_REPLY_STRING ||
        reply->type == REDIS_REPLY_STATUS ||
        reply->type == REDIS_REPLY_ERROR)
    {
        str += '"' + string(reply->str, reply->len) + '"';
    }
    else if (reply->type == REDIS_REPLY_INTEGER)
    {
        str += toString(reply->integer);
    }
    else if (reply->type == REDIS_REPLY_ARRAY)
    {
        str += toString(reply->elements) + " ";
        for (size_t i = 0; i < reply->elements; i++)
        {
            str += " " + redisReplyToString(reply->element[i]);
        }
    }
    str += " }";

    return str;
}

void connectCallback(hiredis::Hiredis* c, int status)
{
    if (status != REDIS_OK)
    {
        LOG_ERROR << "connectCallback Error:" << c->errstr();
    }
    else
    {
        LOG_INFO << "Connected...";
    }
}

void disconnectCallback(hiredis::Hiredis* c, int status)
{
    if (status != REDIS_OK)
    {
        LOG_ERROR << "disconnectCallback Error:" << c->errstr();
    }
    else
    {
        LOG_INFO << "Disconnected...";
    }
}

void timeCallback(hiredis::Hiredis* c, redisReply* reply)
{
    LOG_INFO << "time " << redisReplyToString(reply);
}

void echoCallback(hiredis::Hiredis* c, redisReply* reply, string* echo)
{
    LOG_INFO << *echo << " " << redisReplyToString(reply);
    c->disconnect();
}

void dbsizeCallback(hiredis::Hiredis* c, redisReply* reply)
{
    LOG_INFO << "dbsize " << redisReplyToString(reply);
}

void selectCallback(hiredis::Hiredis* c, redisReply* reply, uint16_t* index)
{
    LOG_INFO << "select " << *index << " " << redisReplyToString(reply);
}

void authCallback(hiredis::Hiredis* c, redisReply* reply, string* password)
{
    LOG_INFO << "auth " << *password << " " << redisReplyToString(reply);
}

void echo(hiredis::Hiredis* c, string* s)
{
    c->command(std::bind(echoCallback, _1, _2, s), "echo %s", s->c_str());
}

void savePbConnectCallback(hiredis::Hiredis* c, int status)
{
    if (status != REDIS_OK)
    {
        LOG_ERROR << "connectCallback Error:" << c->errstr();
    }
    else
    {
        LOG_INFO << "Connected...";
    }
}

void savePbCallback(hiredis::Hiredis* c, redisReply* reply, google::protobuf::Message& pb)
{
    LOG_INFO << pb.DebugString() << " " << redisReplyToString(reply);
}

void loadPbCallback(hiredis::Hiredis* c, redisReply* reply, google::protobuf::Message& pb)
{
    if (!pb.ParseFromArray(reply->str, static_cast<int32_t>(reply->len)))
    {
        LOG_ERROR << "Failed to parse protobuf from redis reply in loadPbCallback";
    }
    LOG_INFO << pb.DebugString() << " " << redisReplyToString(reply);
}

// ---------------------------------------------------------------------------
// Protobuf 序列化存取 Redis（异步回调模式）
// ---------------------------------------------------------------------------

TEST(HiredisLifecycleTest, DestructorRemovesActiveChannelBeforeEventLoopDestruction)
{
	EventLoop loop;
	InetAddress serverAddr("127.0.0.1", 1);
	{
		// 不运行 loop,故意让 Hiredis 在 Channel 仍注册时析构。
		// 回归点是 redisAsyncFree -> ev.cleanup 必须先注销 Channel,
		// 不能触发旧析构顺序的 active-channel 断言。
		hiredis::Hiredis client(&loop, serverAddr);
		client.connect();
	}
	SUCCEED();
}

TEST(HiredisAsyncTest, SaveAndLoadProtobuf)
{
    Logger::setLogLevel(Logger::DEBUG);

    EventLoop loop;

    InetAddress serverAddr("127.0.0.1", 6379);
    hiredis::Hiredis hiredis(&loop, serverAddr);

    hiredis.setConnectCallback(savePbConnectCallback);
    hiredis.setDisconnectCallback(disconnectCallback);
    hiredis.connect();

    std::string key = "luhailong0000000";

    // 1) 延迟 2s 后将 LoginRequest 序列化后 SET 到 Redis
    LoginRequest save_message;
    loop.runAfter(2,[&hiredis, &key, &save_message]() ->void
    {        
        save_message.set_account("luhailon g111");
        save_message.set_password("12 3");
        MessageCachedArray message_cached_array(save_message.ByteSizeLong());
        save_message.SerializeWithCachedSizesToArray(message_cached_array.data());
        hiredis.command(std::bind(savePbCallback, _1, _2, save_message), "SET %b %b", key.c_str(), key.length(),
            message_cached_array.data(),
            message_cached_array.size());
    });
    // 2) 延迟 3s 后从 Redis GET 并反序列化
    LoginRequest load_message;
    loop.runAfter(3, [&hiredis, &key, &load_message]() ->void
    {
        LoginRequest load_message;
        std::string format = std::string("GET ") + key; 
        hiredis.command(std::bind(loadPbCallback, _1, _2, load_message), format.c_str());
    });
   
    loop.loop();
}

// ---------------------------------------------------------------------------
// Hiredis 基本命令（time / echo / dbsize / select / auth）
// ---------------------------------------------------------------------------

TEST(HiredisAsyncTest, BasicCommands)
{
    Logger::setLogLevel(Logger::DEBUG);

    EventLoop loop;

    InetAddress serverAddr("127.0.0.1", 6379);
    hiredis::Hiredis hiredis(&loop, serverAddr);

    hiredis.setConnectCallback(connectCallback);
    hiredis.setDisconnectCallback(disconnectCallback);
    hiredis.connect();

    //hiredis.ping();
    loop.runEvery(1.0, std::bind(&hiredis::Hiredis::ping, &hiredis));

    hiredis.command(timeCallback, "time");

    string hi = "hi";
    hiredis.command(std::bind(echoCallback, _1, _2, &hi), "echo %s", hi.c_str());
    loop.runEvery(2.0, std::bind(echo, &hiredis, &hi));

    hiredis.command(dbsizeCallback, "dbsize");

    uint16_t index = 8;
    hiredis.command(std::bind(selectCallback, _1, _2, &index), "select %d", index);

    string password = "password";
    hiredis.command(std::bind(authCallback, _1, _2, &password), "auth %s", password.c_str());

    loop.loop();
}


int main(int argc, char** argv)
{
    testing::InitGoogleTest(&argc, argv);

    return RUN_ALL_TESTS();
}
