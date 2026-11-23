package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestWaitCapacityIsExplicitlyConfigurable(t *testing.T) {
	cfg, err := Load(strings.NewReader(`{"database":"/data/hub.db","public_url":"https://message.example.test","max_concurrent_waits":3}`))
	if err != nil {
		t.Fatalf("explicit wait capacity rejected: %v", err)
	}
	value := reflect.ValueOf(cfg).FieldByName("MaxConcurrentWaits")
	if !value.IsValid() || value.Int() != 3 {
		t.Fatal("wait capacity not represented")
	}
	for _, value := range []string{"0", "-1", "129"} {
		if _, err := Load(strings.NewReader(`{"database":"/data/hub.db","public_url":"https://message.example.test","max_concurrent_waits":` + value + `}`)); err == nil {
			t.Fatal("unbounded wait capacity accepted")
		}
	}
}
