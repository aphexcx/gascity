package runtime

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNudgeIdentityStaysOffModelWire(t *testing.T) {
	content := NudgeContent(WithNudgeIdentity(context.Background(), "delivery-A"), "hello")
	if NudgeIdentity(content) != "delivery-A" || FlattenText(content) != "hello" {
		t.Fatal("transport identity changed the message or was lost")
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `[{"type":"text","text":"hello"}]` {
		t.Fatalf("delivery identity leaked onto the content wire: %s", raw)
	}
	var untrusted []ContentBlock
	if err := json.Unmarshal([]byte(`[{"type":"text","text":"hello","nudgeID":"delivery-A","delivery_id":"delivery-A"}]`), &untrusted); err != nil {
		t.Fatal(err)
	}
	if NudgeIdentity(untrusted) != "" || NudgeIdentity(NudgeContent(context.Background(), "hello")) != "" {
		t.Fatal("wire input manufactured an internal delivery identity")
	}
}
