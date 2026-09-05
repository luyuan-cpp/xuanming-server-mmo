#include <gtest/gtest.h>
#include <chrono>
#include <thread>

#include "time/system/time.h"

#include <boost/date_time/posix_time/posix_time.hpp>

#include "muduo/base/CrossPlatformAdapterFunction.h"

void PrintUTCWithMilliseconds() {
	uint64_t milliseconds = TimeSystem::NowMillisecondsUTC();
	boost::posix_time::ptime utc_now = boost::posix_time::microsec_clock::universal_time();

	std::cout << "Current UTC time: " << boost::posix_time::to_simple_string(utc_now) << std::endl;
	std::cout << "Milliseconds since 1970-01-01: " << milliseconds << std::endl;
}

void PrintUTCWithSeconds() {
	uint64_t seconds = TimeSystem::NowSecondsUTC();
	boost::posix_time::ptime utc_now = boost::posix_time::second_clock::universal_time();

	// print UTC time
	std::cout << "Current UTC time: " << boost::posix_time::to_simple_string(utc_now) << std::endl;
	std::cout << "Seconds since 1970-01-01: " << seconds << std::endl;
}

TEST(TimeUtilTest, NowMilliseconds) {
	uint64_t localMilliseconds = TimeSystem::NowMilliseconds();
	EXPECT_GE(localMilliseconds, 0);  // return value should be >= 0
}

TEST(TimeUtilTest, NowSeconds) {
	uint64_t localSeconds = TimeSystem::NowSeconds();
	EXPECT_GE(localSeconds, 0);  // return value should be >= 0
}

TEST(TimeUtilTest, NowMillisecondsUTC) {
	uint64_t utcMilliseconds = TimeSystem::NowMillisecondsUTC();
	EXPECT_GE(utcMilliseconds, 0);  // return value should be >= 0
}

TEST(TimeUtilTest, NowSecondsUTC) {
	uint64_t utcSeconds = TimeSystem::NowSecondsUTC();
	EXPECT_GE(utcSeconds, 0);  // return value should be >= 0
}

// Compare local time and UTC time (milliseconds)
TEST(TimeUtilTest, CompareLocalAndUTC) {
	uint64_t localMilliseconds = TimeSystem::NowMilliseconds();
	uint64_t utcMilliseconds = TimeSystem::NowMillisecondsUTC();

	// difference between local and UTC should be close to 8 hours
	int64_t difference = static_cast<int64_t>(localMilliseconds) - static_cast<int64_t>(utcMilliseconds);

	// 8 hours in milliseconds
	const uint64_t eightHoursInMilliseconds = 8 * 60 * 60 * 1000;

	EXPECT_LE(difference, eightHoursInMilliseconds);  // local time should be <= UTC + 8 hours
	EXPECT_GE(difference, 0);  // local time should be >= UTC time
}

// Compare local time and UTC time (seconds)
TEST(TimeUtilTest, CompareLocalAndUTCInSeconds) {
	uint64_t localSeconds = TimeSystem::NowSeconds();
	uint64_t utcSeconds = TimeSystem::NowSecondsUTC();

	// difference between local and UTC should be close to 8 hours
	int64_t difference = static_cast<int64_t>(localSeconds) - static_cast<int64_t>(utcSeconds);

	// 8 hours in seconds
	const uint64_t eightHoursInSeconds = 8 * 60 * 60;

	EXPECT_LE(difference, eightHoursInSeconds);  // local time should be <= UTC + 8 hours
	EXPECT_GE(difference, 0);  // local time should be >= UTC time
}

int main(int argc, char** argv) {
	::testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}