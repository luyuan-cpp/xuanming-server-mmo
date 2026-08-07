// Copyright 2011, Shuo Chen.  All rights reserved.
// http://code.google.com/p/muduo/
//
// Use of this source code is governed by a BSD-style license
// that can be found in the License file.
//
// Author: Shuo Chen (chenshuo at chenshuo dot com)

#ifndef MUDUO_EXAMPLES_PROTOBUF_CODEC_CODEC_H
#define MUDUO_EXAMPLES_PROTOBUF_CODEC_CODEC_H

#include "muduo/net/Buffer.h"
#include "muduo/net/TcpConnection.h"

#include <google/protobuf/message.h>

// struct ProtobufTransportFormat __attribute__ ((__packed__))
// {
//   int32_t  len;
//   int32_t  nameLen;
//   char     typeName[nameLen];
//   char     protobufData[len-nameLen-8];
//   int32_t  checkSum; // adler32 of nameLen, typeName and protobufData
// }

typedef std::shared_ptr<google::protobuf::Message> MessagePtr;

//
// FIXME: merge with RpcCodec
//
class ProtobufCodec : muduo::noncopyable
{
 public:

  enum ErrorCode
  {
    kNoError = 0,
    kInvalidLength,
    kCheckSumError,
    kInvalidNameLen,
    kUnknownMessageType,
    kParseError,
  };

  typedef std::function<void (const muduo::net::TcpConnectionPtr&,
                                const MessagePtr&,
                                muduo::Timestamp)> ProtobufMessageCallback;

  typedef std::function<void (const muduo::net::TcpConnectionPtr&,
                                muduo::net::Buffer*,
                                muduo::Timestamp,
                                ErrorCode)> ErrorCallback;

  // maxMessageLen:单条消息的硬上限,超过即 kInvalidLength → 断连。
  //
  // 默认值刻意收紧到 64KB 而不是老的 64MB。本 codec 的唯一使用者是 gate 的
  // **客户端-facing** 监听(节点间走 RpcCodec,与此无关),而客户端合法消息
  // 上限是 1KB(CheckMessageSize 对 ClientRequest 的限制)。老的 64MB 意味着:
  // 长度检查放行之后,恶意客户端一条消息就能让 gate 全量缓冲 63MB、跑一遍
  // adler32、按 typeName 反射建任意已注册 proto 对象再 parse —— 都发生在
  // CheckMessageSize 之前。单连接 64MB × N 条连接,接收缓冲直接把 gate 推到
  // OOM。64KB 给 ClientTokenVerifyRequest 之类的握手报文留了充分余量,
  // 又把单连接的预验证内存占用压回两个数量级。
  static constexpr int kDefaultMaxMessageLen = 64*1024;

  explicit ProtobufCodec(const ProtobufMessageCallback& messageCb,
                         int maxMessageLen = kDefaultMaxMessageLen)
    : messageCallback_(messageCb),
      errorCallback_(defaultErrorCallback),
      maxMessageLen_(maxMessageLen)
  {
  }

  ProtobufCodec(const ProtobufMessageCallback& messageCb, const ErrorCallback& errorCb,
                int maxMessageLen = kDefaultMaxMessageLen)
    : messageCallback_(messageCb),
      errorCallback_(errorCb),
      maxMessageLen_(maxMessageLen)
  {
  }

  void onMessage(const muduo::net::TcpConnectionPtr& conn,
                 muduo::net::Buffer* buf,
                 muduo::Timestamp receiveTime);

  void send(const muduo::net::TcpConnectionPtr& conn,
            const google::protobuf::Message& message)
  {
    // FIXME: serialize to TcpConnection::outputBuffer()
    muduo::net::Buffer buf;
    fillEmptyBuffer(&buf, message);
    conn->send(&buf);
  }

  static const muduo::string& errorCodeToString(ErrorCode errorCode);
  static void fillEmptyBuffer(muduo::net::Buffer* buf, const google::protobuf::Message& message);
  static google::protobuf::Message* createMessage(const std::string& type_name);
  static MessagePtr parse(const char* buf, int len, ErrorCode* errorCode);

 private:
  static void defaultErrorCallback(const muduo::net::TcpConnectionPtr&,
                                   muduo::net::Buffer*,
                                   muduo::Timestamp,
                                   ErrorCode);

  ProtobufMessageCallback messageCallback_;
  ErrorCallback errorCallback_;
  int maxMessageLen_{kDefaultMaxMessageLen};

  const static int kHeaderLen = sizeof(int32_t);
  const static int kMinMessageLen = 2*kHeaderLen + 2; // nameLen + typeName + checkSum
};

#endif  // MUDUO_EXAMPLES_PROTOBUF_CODEC_CODEC_H
