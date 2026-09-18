package store

import (
	"path/filepath"
	"testing"
	"time"

	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
)

// quotaEvidenceStore 构造两个可用账号：两个都有新鲜的模型快照，但只有
// 后一个有计划写入的用量快照。
func quotaEvidenceStore(t *testing.T, snapshots func(views []events.AccountView, now time.Time) map[string]events.AccountUsageSnapshot) (*Store, []string, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.duckdb")
	s, err := Open(path, "256MB", 1, encryptedTestCodec(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, _, _, err = s.Import([]events.CredentialInput{
		{AccessToken: "test-access", RefreshToken: "test-refresh"},
		{AccessToken: "test-access-2", RefreshToken: "test-refresh-2"},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	modelSnapshot := events.AccountModelSnapshot{Models: []events.AccountModelEntry{{ID: "gpt-5.5"}}, DiscoveredAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}
	views := s.List()
	ids := make([]string, 0, len(views))
	for _, view := range views {
		if _, _, err = s.PutModelSnapshot(view.ID, modelSnapshot); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, view.ID)
	}
	for id, snapshot := range snapshots(views, now) {
		if _, err = s.PutUsageSnapshot(id, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	return s, ids, now
}

func usageSnapshot(now time.Time, expiresIn time.Duration, usedPercent float64, resetAt time.Time) events.AccountUsageSnapshot {
	return events.AccountUsageSnapshot{
		ObservedAt: now.Format(time.RFC3339),
		ExpiresAt:  now.Add(expiresIn).Format(time.RFC3339),
		Windows: []events.UsageWindow{{
			ID: "header-primary", Label: "Primary",
			UsedPercent: usedPercent, UsedPercentKnown: true,
			LimitReached:  usedPercent >= 100,
			ResetAt:       resetAt.Format(time.RFC3339),
			WindowSeconds: 43200 * 60,
		}},
	}
}

// CP-SCHED-011: 有新鲜额度证据且仍有余量的账号，优先于额度未知的账号，即使
// 轮转游标本应先命中后者。
func TestAcquirePrefersFreshQuotaHeadroom(t *testing.T) {
	var preferredID string
	s, ids, _ := quotaEvidenceStore(t, func(views []events.AccountView, now time.Time) map[string]events.AccountUsageSnapshot {
		// 未知额度的是 ids[1]（转游标起点），有余量证据的是 ids[0]。
		preferredID = views[0].ID
		return map[string]events.AccountUsageSnapshot{
			views[0].ID: usageSnapshot(now, 15*time.Minute, 26, now.Add(30*24*time.Hour)),
		}
	})

	got, err := s.AcquirePreferredTransport("gpt-5.5", nil, "", "responses")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != preferredID {
		t.Fatalf("CP-SCHED-011 selected unknown-quota account: got=%s want=%s (all=%v)", got.AccountID, preferredID, ids)
	}
}

// CP-SCHED-011: 全部候选都缺少新鲜额度证据时，轮转行为与引入本规则前一致。
func TestAcquireKeepsRotationWhenNoQuotaEvidence(t *testing.T) {
	s, ids, _ := quotaEvidenceStore(t, func([]events.AccountView, time.Time) map[string]events.AccountUsageSnapshot {
		return nil
	})
	first, err := s.AcquirePreferredTransport("gpt-5.5", nil, "", "responses")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AcquirePreferredTransport("gpt-5.5", nil, "", "responses")
	if err != nil {
		t.Fatal(err)
	}
	if first.AccountID == second.AccountID {
		t.Fatalf("CP-SCHED-001 rotation stalled on %s", first.AccountID)
	}
	if first.AccountID != ids[0] || second.AccountID != ids[1] {
		t.Fatalf("CP-SCHED-001 rotation order changed: %s then %s (all=%v)", first.AccountID, second.AccountID, ids)
	}
}

// CP-SCHED-011 只认可新鲜证据：过期的额度快照既不再阻止尝试（CP-CAP-005），
// 也不能在排序上压过另一个有新鲜余量证据的候选。
func TestAcquireDoesNotRankExpiredQuotaSnapshot(t *testing.T) {
	var expiredID, headroomID string
	s, _, _ := quotaEvidenceStore(t, func(views []events.AccountView, now time.Time) map[string]events.AccountUsageSnapshot {
		expiredID, headroomID = views[0].ID, views[1].ID
		return map[string]events.AccountUsageSnapshot{
			// 轮转游标从 views[0] 开始；它的快照已过期，即使写着 LimitReached 也不算证据。
			views[0].ID: usageSnapshot(now.Add(-2*time.Hour), time.Minute, 100, now.Add(30*24*time.Hour)),
			views[1].ID: usageSnapshot(now, 15*time.Minute, 26, now.Add(30*24*time.Hour)),
		}
	})
	got, err := s.AcquirePreferredTransport("gpt-5.5", nil, "", "responses")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != headroomID {
		t.Fatalf("CP-SCHED-011 expired snapshot outranked fresh evidence: got=%s want=%s (expired=%s)", got.AccountID, headroomID, expiredID)
	}
}

// CP-SCHED-004/CP-SCHED-011: 粘性账号仍然优先于额度证据排序。
func TestAcquireKeepsStickyAccountOverQuotaEvidence(t *testing.T) {
	var headroomID, stickyID string
	s, _, _ := quotaEvidenceStore(t, func(views []events.AccountView, now time.Time) map[string]events.AccountUsageSnapshot {
		headroomID, stickyID = views[0].ID, views[1].ID
		return map[string]events.AccountUsageSnapshot{
			views[0].ID: usageSnapshot(now, 15*time.Minute, 26, now.Add(30*24*time.Hour)),
		}
	})
	got, err := s.AcquirePreferredTransport("gpt-5.5", nil, stickyID, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != stickyID {
		t.Fatalf("CP-SCHED-004 stickiness lost to quota evidence: got=%s want=%s (headroom=%s)", got.AccountID, stickyID, headroomID)
	}
}

// CP-CAP-005 / CP-SCHED-011: 新鲜且未到期的 limit-reached 快照仍然硬性排除。
func TestAcquireStillBlocksFreshLimitReachedSnapshot(t *testing.T) {
	var blockedID, otherID string
	s, _, _ := quotaEvidenceStore(t, func(views []events.AccountView, now time.Time) map[string]events.AccountUsageSnapshot {
		blockedID, otherID = views[0].ID, views[1].ID
		return map[string]events.AccountUsageSnapshot{
			views[0].ID: usageSnapshot(now, 15*time.Minute, 100, now.Add(30*24*time.Hour)),
			views[1].ID: usageSnapshot(now, 15*time.Minute, 26, now.Add(30*24*time.Hour)),
		}
	})
	got, err := s.AcquirePreferredTransport("gpt-5.5", nil, blockedID, "responses")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != otherID {
		t.Fatalf("CP-CAP-005 fresh limit-reached account was selected: got=%s blocked=%s", got.AccountID, blockedID)
	}
}
