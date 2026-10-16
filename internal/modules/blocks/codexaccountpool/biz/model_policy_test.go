package biz

import (
	"testing"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	"github.com/muidea/magicCommon/event"
)

func TestModelBlacklistOverridesStickySession(t *testing.T) {
	pool, _, ids := newSchedulingAccount(t)
	cmd := events.AcquireCommand{Model: "gpt-test", SessionHash: "session", PreferredID: ids[0]}
	first := acquireForTest(t, pool, cmd)
	releaseForTest(t, pool, first.LeaseID)
	result := event.NewResult(events.TopicUpdate, "test", pool.ID())
	pool.handleUpdate(event.NewEvent(events.TopicUpdate, "test", pool.ID(), nil, events.UpdateCommand{ID: first.AccountID, BlockedModels: events.ModelBlacklist{"gpt-test"}}), result)
	if _, err := result.Get(); err != nil {
		t.Fatal(err)
	}
	second := acquireForTest(t, pool, cmd)
	if second.AccountID == first.AccountID {
		t.Fatal("sticky binding bypassed blacklist")
	}
	releaseForTest(t, pool, second.LeaseID)
	checked := event.NewResult(events.TopicCheckModelPolicy, "test", pool.ID())
	pool.handleCheckModelPolicy(event.NewEvent(events.TopicCheckModelPolicy, "test", pool.ID(), nil, events.CheckModelPolicyCommand{AccountID: first.AccountID, Model: "gpt-test"}), checked)
	value, err := checked.Get()
	if err != nil || value.(events.CheckModelPolicyResult).Allowed {
		t.Fatalf("policy=%+v %v", value, err)
	}
}
