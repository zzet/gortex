package store_sqlite

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

func dedicatedRetryRequest(claim DedicatedBaseBuildClaim, token string) ClaimDedicatedBaseBuildRequest {
	return ClaimDedicatedBaseBuildRequest{
		Desire: claim.Desire, AttemptToken: token,
		ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
		BaseGenerationID:           claim.BaseGenerationID, LayerID: claim.LayerID,
		LowerViewFingerprint: claim.LowerViewFingerprint, CreatedAt: 1,
	}
}

func installDedicatedRetryAudit(t testing.TB, f *dedicatedPublicationFixture) func() int64 {
	t.Helper()
	f.exec(t, `CREATE TABLE retry_write_audit(n INTEGER NOT NULL)`)
	f.exec(t, `INSERT INTO retry_write_audit VALUES(0)`)
	for _, table := range []string{"view_generations", "dedicated_base_publications", "dedicated_graphs"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			f.exec(t, fmt.Sprintf(`CREATE TRIGGER retry_%s_%s AFTER %s ON %s BEGIN UPDATE retry_write_audit SET n=n+1; END`, table, event, event, table))
		}
	}
	return func() int64 {
		var n int64
		if err := f.store.db.QueryRow(`SELECT n FROM retry_write_audit`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func TestDedicatedPublicationTerminalPayloadRecovery(t *testing.T) {
	for _, delta := range []bool{false, true} {
		t.Run(fmt.Sprintf("delta=%v", delta), func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			ctx := context.Background()
			var base int64
			if delta {
				initial := f.claim(t, "initial", 0, 0)
				f.publish(t, initial)
				f.adopt(t, initial)
				base = initial.GenerationID
				identity := f.desire.Identity
				identity.TreeOID = "tree-b"
				f.observe(t, identity)
			}
			old := f.claim(t, "lost-failure-notification", base, base)
			if err := f.c.SetViewGenerationState(ctx, old.GenerationID, ViewGenerationFailed); err != nil {
				t.Fatal(err)
			}
			audit := installDedicatedRetryAudit(t, f)
			validate := dedicatedRetryRequest(old, old.AttemptToken)
			validate.ExistingGenerationID = old.GenerationID
			if _, err := f.c.ClaimDedicatedBaseBuild(ctx, validate); err == nil {
				t.Fatal("validation-only claim accepted a failed physical generation")
			}
			if _, err := f.c.ClaimDedicatedBaseBuild(ctx, dedicatedRetryRequest(old, old.AttemptToken)); err == nil {
				t.Fatal("recovery reused a retired attempt token")
			}
			if audit() != 0 {
				t.Fatal("refused recovery performed DML")
			}
			count := f.count(t)
			retry, err := f.c.ClaimDedicatedBaseBuild(ctx, dedicatedRetryRequest(old, "retry"))
			if err != nil {
				t.Fatal(err)
			}
			if retry.GenerationID <= old.GenerationID || retry.Desire != old.Desire || retry.AttemptToken == old.AttemptToken || retry.Status != "allocated" || f.count(t) != count+1 || f.active(t) != base {
				t.Fatalf("invalid retry: old=%+v retry=%+v", old, retry)
			}
			// One new metadata row and one replacement binding; no intermediate
			// failure UPDATE, payload rewrite, or active-pointer publication.
			if got := audit(); got != 2 {
				t.Fatalf("recovery DML=%d, want2", got)
			}
			row, found, err := f.c.GetViewGeneration(ctx, old.GenerationID)
			if err != nil || !found || row.State != ViewGenerationFailed {
				t.Fatalf("recovery resurrected old payload: %+v found=%v err=%v", row, found, err)
			}
			f.publish(t, retry)
			f.adopt(t, retry)
			if f.active(t) != retry.GenerationID {
				t.Fatal("replacement cannot adopt")
			}
		})
	}
}

func TestDedicatedPublicationTerminalPayloadRecoveryGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *dedicatedPublicationFixture, DedicatedBaseBuildClaim) DedicatedBaseBuildClaim
	}{
		{"missing", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `DELETE FROM view_generations WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"retiring", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET state='retiring' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_tree", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET tree_oid='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_config", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET config_hash='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_extractors", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET extractor_versions='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_resolver", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET resolver_version='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_owner_kind", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET owner_kind='checkout' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_checkout", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET checkout_id='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_parent", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET base_generation_id=? WHERE generation_id=?`, c.GenerationID, c.GenerationID)
			return c
		}},
		{"wrong_layer", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET layer_id='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"wrong_lower", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE view_generations SET lower_view_fingerprint='other' WHERE generation_id=?`, c.GenerationID)
			return c
		}},
		{"ready_attempt_failed_payload", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE dedicated_base_publications SET attempt_state='ready' WHERE graph_id=?`, f.graph.GraphID)
			return c
		}},
		{"before_authority_floor", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE dedicated_base_publications SET owner_generation_floor=? WHERE graph_id=?`, c.GenerationID, f.graph.GraphID)
			c.Desire.Authority.GenerationFloor = c.GenerationID
			return c
		}},
		{"stale_expected_active", func(t *testing.T, f *dedicatedPublicationFixture, c DedicatedBaseBuildClaim) DedicatedBaseBuildClaim {
			f.exec(t, `UPDATE dedicated_base_publications SET expected_active_generation_id=? WHERE graph_id=?`, c.GenerationID, f.graph.GraphID)
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			ctx := context.Background()
			old := f.claim(t, "first", 0, 0)
			if err := f.c.SetViewGenerationState(ctx, old.GenerationID, ViewGenerationFailed); err != nil {
				t.Fatal(err)
			}
			old = tc.mutate(t, f, old)
			count := f.count(t)
			audit := installDedicatedRetryAudit(t, f)
			if _, err := f.c.ClaimDedicatedBaseBuild(ctx, dedicatedRetryRequest(old, "retry")); err == nil {
				t.Fatal("invalid association recovered")
			}
			if audit() != 0 || f.count(t) != count || f.active(t) != 0 {
				t.Fatal("invalid recovery changed logical state")
			}
		})
	}
}

func TestDedicatedPublicationTerminalPayloadRecoveryRejectsFailedAncestor(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	base := f.claim(t, "base", 0, 0)
	f.publish(t, base)
	f.adopt(t, base)
	identity := f.desire.Identity
	identity.TreeOID = "tree-b"
	f.observe(t, identity)
	child := f.claim(t, "child", base.GenerationID, base.GenerationID)
	for _, id := range []int64{base.GenerationID, child.GenerationID} {
		if err := f.c.SetViewGenerationState(ctx, id, ViewGenerationFailed); err != nil {
			t.Fatal(err)
		}
	}
	audit := installDedicatedRetryAudit(t, f)
	if _, err := f.c.ClaimDedicatedBaseBuild(ctx, dedicatedRetryRequest(child, "retry")); err == nil {
		t.Fatal("failed ancestor accepted for recovery")
	}
	if audit() != 0 {
		t.Fatal("failed ancestry refusal wrote data")
	}
}

func TestDedicatedPublicationTerminalPayloadRecoverySingleAllocation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	old := f.claim(t, "old", 0, 0)
	if err := f.c.SetViewGenerationState(ctx, old.GenerationID, ViewGenerationFailed); err != nil {
		t.Fatal(err)
	}
	const callers = 12
	claims := make([]DedicatedBaseBuildClaim, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claims[i], errs[i] = f.c.ClaimDedicatedBaseBuild(ctx, dedicatedRetryRequest(old, fmt.Sprint("retry-", i)))
		}()
	}
	wg.Wait()
	allocated := 0
	for i, claim := range claims {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if claim.GenerationID <= old.GenerationID || claim.GenerationID != claims[0].GenerationID || claim.AttemptToken != claims[0].AttemptToken {
			t.Fatalf("uncoalesced recovery: %+v", claims)
		}
		if claim.Status == "allocated" {
			allocated++
		}
	}
	if allocated != 1 || f.count(t) != 2 {
		t.Fatalf("allocations=%d generations=%d", allocated, f.count(t))
	}
}

func BenchmarkDedicatedPublicationBuildingCoalesce(b *testing.B) {
	f := newDedicatedPublicationFixture(b)
	claim := f.claim(b, "leader", 0, 0)
	ctx := context.Background()
	req := dedicatedRetryRequest(claim, "follower")
	audit := installDedicatedRetryAudit(b, f)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := f.c.ClaimDedicatedBaseBuild(ctx, req)
		if err != nil || got.GenerationID != claim.GenerationID || got.Status != "building" {
			b.Fatalf("coalescing=%+v err=%v", got, err)
		}
	}
	b.StopTimer()
	if audit() != 0 {
		b.Fatal("healthy coalescing performed DML")
	}
}

func BenchmarkDedicatedPublicationTerminalPayloadRecovery(b *testing.B) {
	f := newDedicatedPublicationFixture(b)
	ctx := context.Background()
	claim := f.claim(b, "initial", 0, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if err := f.c.SetViewGenerationState(ctx, claim.GenerationID, ViewGenerationFailed); err != nil {
			b.Fatal(err)
		}
		req := dedicatedRetryRequest(claim, fmt.Sprint("retry-", i))
		b.StartTimer()
		var err error
		claim, err = f.c.ClaimDedicatedBaseBuild(ctx, req)
		if err != nil || claim.Status != "allocated" {
			b.Fatalf("recovery=%+v err=%v", claim, err)
		}
	}
	b.StopTimer()
	if f.count(b) != b.N+1 {
		b.Fatal("recovery generation accounting")
	}
}
