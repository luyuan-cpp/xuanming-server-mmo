package authoritysql

// IdleProbeAfterForTest 把空闲探测窗口交给外部测试包(authoritysql_test 的真库用例),免得那边再抄一份数值。
const IdleProbeAfterForTest = idleProbeAfter
