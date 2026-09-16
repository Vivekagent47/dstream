package outbound

import "testing"

func TestValidateEndpointHeaders(t *testing.T) {
	ok := map[string]map[string]string{
		"empty":  {},
		"simple": {"X-Consumer-Auth": "Bearer abc", "X-Tenant": "acme"},
	}
	for name, h := range ok {
		if err := validateEndpointHeaders(h); err != nil {
			t.Errorf("%s: want ok, got %v", name, err)
		}
	}
	bad := map[string]map[string]string{
		"reserved-ct":      {"Content-Type": "text/plain"},
		"reserved-wh":      {"webhook-id": "x"},
		"reserved-wh-case": {"Webhook-Signature": "x"},
		"reserved-dstream": {"Dstream-Event-Id": "x"},
		"bad-token":        {"X Bad Name": "v"},
	}
	for name, h := range bad {
		if err := validateEndpointHeaders(h); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
	// count cap
	over := map[string]string{}
	for i := 0; i < 21; i++ {
		over["X-H-"+string(rune('a'+i))] = "v"
	}
	if err := validateEndpointHeaders(over); err == nil {
		t.Error("count cap: want error")
	}
	// size cap
	big := map[string]string{"X-Big": string(make([]byte, 4096))}
	if err := validateEndpointHeaders(big); err == nil {
		t.Error("size cap: want error")
	}
}

func TestValidateChannels(t *testing.T) {
	if err := validateChannels(nil); err != nil {
		t.Errorf("nil: %v", err)
	}
	if err := validateChannels([]string{"acme", "eu-west_1", "v1.2"}); err != nil {
		t.Errorf("ok: %v", err)
	}
	if err := validateChannels([]string{"bad space"}); err == nil {
		t.Error("space: want error")
	}
	over := make([]string, 11)
	for i := range over {
		over[i] = "c"
	}
	if err := validateChannels(over); err == nil {
		t.Error("count: want error")
	}
}
