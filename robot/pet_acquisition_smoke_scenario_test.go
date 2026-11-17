package main

import "testing"

// 冒烟会给测试号准备银两，因此连接前必须挡住非本地地址。
func TestPetAcquisitionSmokeRejectsNonLocalGateway(t *testing.T) {
	for _, address := range []string{"", "http://example.com:8081", "http://127.0.0.1.example.com:8081", "https://127.0.0.1:8081", "http://user:secret@127.0.0.1:8081"} {
		if err := validatePetAcquisitionEndpoint(address); err == nil {
			t.Fatalf("unexpected allowed endpoint %q", address)
		}
	}
	if err := validatePetAcquisitionEndpoint("http://127.0.0.1:8081"); err != nil {
		t.Fatal(err)
	}
}
