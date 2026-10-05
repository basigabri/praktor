package agent

import "testing"

func TestFallbackReplyMeta(t *testing.T) {
	telegram := map[string]string{"sender": "user:1", "chat_id": "1"}
	if got := fallbackReplyMeta(telegram); got["chat_id"] != "1" {
		t.Errorf("telegram meta = %v, want it kept", got)
	}
	api := map[string]string{"sender": "user:api", MetaChannel: ChannelAPI, "request_id": "r1"}
	if got := fallbackReplyMeta(api); got != nil {
		t.Errorf("api meta = %v, want nil", got)
	}
	if got := fallbackReplyMeta(nil); got != nil {
		t.Errorf("nil meta = %v, want nil", got)
	}
}
