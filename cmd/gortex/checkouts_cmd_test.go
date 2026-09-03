package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// The verbs are wiring: they lower flags into one tool call and render what
// came back. So the tests stub the relay and assert both halves — the tool
// name and arguments that went out, and the text that came back out — without
// a daemon anywhere.

func newCheckoutsTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	checkoutsIndex = "."
	checkoutsFormat = "text"
	checkoutsFamily = ""
	checkoutsConfirm = false

	buf := &bytes.Buffer{}
	cmd := &cobra.Command{Use: "checkouts"}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// stubCheckoutsTool installs a relay for the duration of one test.
func stubCheckoutsTool(t *testing.T, fn func(string, string, map[string]any) (json.RawMessage, error)) {
	t.Helper()
	orig := checkoutsDaemonTool
	t.Cleanup(func() { checkoutsDaemonTool = orig })
	checkoutsDaemonTool = fn
}

func TestReposFamiliesRendersTheListing(t *testing.T) {
	var gotTool string
	var gotArgs map[string]any
	stubCheckoutsTool(t, func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		gotTool, gotArgs = tool, args
		return json.RawMessage(`{"families":[{
			"family_id":"family-1","common_dir":"/repo/.git","state":"family_ready",
			"primary_epoch":2,"primary_graph_id":"graph-1","primary_repo_prefix":"repo",
			"graphs":[{"graph_id":"graph-1","repo_prefix":"repo","is_primary":true,
				"state":"graph_ready","active_generation_id":7,"served":true}],
			"checkouts":[{"checkout_id":"c1","admin_name":"wt","root_path":"/repo/wt",
				"state":"checkout_ready","desired_mode":"automatic","effective_mode":"automatic",
				"coordinator_live":true,"intents":["cli_track"],
				"route":{"graph_id":"graph-1","commit_generation_id":5,"dirty_generation_id":6,
					"route_epoch":3,"state":"active","ready":true},
				"removal":{"started_at":100,"deadline":200,"evidence":"root_deleted","running":true},
				"evidence":{"present":true,"sampled_at":50,"sample_generation":2}}],
			"ref_views":[{"selector_kind":"git_ref","selector_value":"refs/heads/main",
				"state":"ready","active_tree":"tree-a","desired_tree":"tree-a","last_selected":90}]}]}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, runReposFamilies(cmd, nil))
	require.Equal(t, "list_checkouts", gotTool)
	require.Empty(t, gotArgs, "no filter means no family argument")

	out := buf.String()
	require.Contains(t, out, "family family-1")
	require.Contains(t, out, "primary: repo  graph-1  epoch 2")
	require.Contains(t, out, "graph  repo")
	require.Contains(t, out, "checkout wt")
	require.Contains(t, out, "route=graph-1 epoch=3 active commit=5 dirty=6 ready=true")
	require.Contains(t, out, "removal:")
	require.Contains(t, out, "evidence:")
	require.Contains(t, out, "view   git_ref:refs/heads/main")
}

func TestReposFamiliesRendersTheHeadOfEachCheckout(t *testing.T) {
	cases := []struct {
		name     string
		headJSON string
		want     string
	}{
		{
			name:     "attached",
			headJSON: `"head_ref":"refs/heads/x","head_commit":"1a46dd5e9c4b7f2013e5c6d7a8b9c0d1e2f30411"`,
			want:     "head=refs/heads/x",
		},
		{
			name:     "detached",
			headJSON: `"head_commit":"1a46dd5e9c4b7f2013e5c6d7a8b9c0d1e2f30411"`,
			want:     "head=detached@1a46dd5e9c4b",
		},
		{
			name:     "unsampled",
			headJSON: `"state":"checkout_ready"`,
			want:     "head=(none)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCheckoutsTool(t, func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
				return json.RawMessage(`{"families":[{"family_id":"family-1",
					"checkouts":[{"checkout_id":"c1","admin_name":"wt","root_path":"/repo/wt",
						"effective_mode":"automatic","desired_mode":"automatic",` +
					tc.headJSON + `}]}]}`), nil
			})
			cmd, buf := newCheckoutsTestCmd(t)
			require.NoError(t, runReposFamilies(cmd, nil))
			require.Contains(t, buf.String(), tc.want)
		})
	}
}

func TestReposFamiliesForwardsTheFilterAndJSON(t *testing.T) {
	var gotArgs map[string]any
	stubCheckoutsTool(t, func(_ string, _ string, args map[string]any) (json.RawMessage, error) {
		gotArgs = args
		return json.RawMessage(`{"families":[]}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	checkoutsFamily = "/repo/wt"
	checkoutsFormat = "json"
	require.NoError(t, runReposFamilies(cmd, nil))
	require.Equal(t, map[string]any{"family": "/repo/wt"}, gotArgs)
	require.Contains(t, buf.String(), `"families": []`)
}

func TestReposFamiliesRendersNestedBudgetTruncationTruthfully(t *testing.T) {
	stubCheckoutsTool(t, func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{
			"_truncated_by_budget":true,
			"families":[{
				"family_id":"family-1","common_dir":"/repo/.git","state":"family_ready",
				"primary_epoch":2,"primary_graph_id":"graph-1","primary_repo_prefix":"repo",
				"_truncated_by_budget":true,
				"_max_returned_checkouts":1,"_original_count_checkouts":257,
				"graphs":[{"graph_id":"graph-1","repo_prefix":"repo","is_primary":true,
					"state":"graph_ready","served":true}],
				"checkouts":[{"checkout_id":"c-main","admin_name":"@main","root_path":"/repo",
					"state":"checkout_ready","desired_mode":"dedicated","effective_mode":"dedicated"}]
			}]
		}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, runReposFamilies(cmd, nil))
	out := buf.String()
	require.Contains(t, out, "family family-1")
	require.Contains(t, out, "checkout @main")
	require.Contains(t, out, "response budget: showing 1 of 257 checkouts")
	require.NotContains(t, out, "(no checkout families)")
}

func TestReposFamiliesNeverCallsABudgetedOuterEmptyCatalogEmpty(t *testing.T) {
	stubCheckoutsTool(t, func(_ string, _ string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{
			"_truncated_by_budget":true,
			"_max_returned_families":0,
			"_original_count_families":1,
			"families":[]
		}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, runReposFamilies(cmd, nil))
	out := buf.String()
	require.Contains(t, out, "listing truncated by response budget")
	require.Contains(t, out, "showing 0 of 1 checkout families")
	require.NotContains(t, out, "(no checkout families)")
}

func TestReposSetPrimaryPreviewsUntilConfirmed(t *testing.T) {
	var gotArgs map[string]any
	stubCheckoutsTool(t, func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		require.Equal(t, "set_primary_checkout", tool)
		gotArgs = args
		if args["confirm"] == true {
			return json.RawMessage(`{"status":"primary_set","family_id":"family-1",
				"graph_id":"graph-2","repo_prefix":"repo@wt","primary_epoch":3,
				"rebuilt":["c1"],"stale":["c2"]}`), nil
		}
		return json.RawMessage(`{"status":"preview","family_id":"family-1",
			"graph_id":"graph-2","repo_prefix":"repo@wt","current_graph_id":"graph-1",
			"current_repo_prefix":"repo","primary_epoch":2,"ready":true,
			"confirm_required":true,
			"dependents":[{"kind":"checkout","id":"c1","detail":"checkout wt rebuilds its layers over repo@wt"}]}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, runReposSetPrimary(cmd, []string{"repo@wt"}))
	require.Equal(t, map[string]any{"graph": "repo@wt"}, gotArgs,
		"a preview must not send a confirm")
	out := buf.String()
	require.Contains(t, out, "preview: repo@wt would become the primary corpus")
	require.Contains(t, out, "currently: repo  graph-1")
	require.Contains(t, out, "rebuilds checkout:")
	require.Contains(t, out, "gortex repos set-primary graph-2 --confirm")

	cmd, buf = newCheckoutsTestCmd(t)
	checkoutsConfirm = true
	require.NoError(t, runReposSetPrimary(cmd, []string{"repo@wt"}))
	require.Equal(t, true, gotArgs["confirm"])
	out = buf.String()
	require.Contains(t, out, "primary set: repo@wt is the base corpus of family family-1")
	require.Contains(t, out, "rebuilt 1 checkouts")
	require.Contains(t, out, "stale: checkout c2 kept its old route")
}

func TestReposForgetPreviewsUntilConfirmed(t *testing.T) {
	var gotArgs map[string]any
	stubCheckoutsTool(t, func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		require.Equal(t, "forget_checkout", tool)
		gotArgs = args
		if args["confirm"] == true {
			return json.RawMessage(`{"status":"forgotten","plan":"forget","prefix":"repo@wt",
				"nodes_removed":12,"edges_removed":34,"revoked_intents":["cli_track"]}`), nil
		}
		return json.RawMessage(`{"status":"preview","action":"forget","plan":"forget",
			"prefix":"repo@wt","confirm_required":true,
			"detail":"nothing was written",
			"closure":[{"kind":"graph","id":"graph-2","detail":"corpus repo@wt is retired with the checkout"}]}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, runReposForget(cmd, []string{"/repo/wt"}))
	require.Equal(t, map[string]any{"path": "/repo/wt"}, gotArgs,
		"a preview must not send a confirm")
	out := buf.String()
	require.Contains(t, out, `preview: /repo/wt would run the "forget" plan`)
	require.Contains(t, out, "removes graph: corpus repo@wt is retired")
	require.Contains(t, out, "run: gortex repos forget /repo/wt --confirm")

	cmd, buf = newCheckoutsTestCmd(t)
	checkoutsConfirm = true
	require.NoError(t, runReposForget(cmd, []string{"/repo/wt"}))
	require.Equal(t, true, gotArgs["confirm"])
	out = buf.String()
	require.Contains(t, out, "forgotten: /repo/wt (forget plan)")
	require.Contains(t, out, "revoked intents: cli_track")
	require.Contains(t, out, "removed 12 nodes, 34 edges")
}

func TestReposReconcileScopesToOneFamily(t *testing.T) {
	var gotArgs map[string]any
	stubCheckoutsTool(t, func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		require.Equal(t, "reconcile_checkouts", tool)
		gotArgs = args
		return json.RawMessage(`{"status":"reconciled","coordinators":1,"retired":2,
			"families":[{"family_id":"family-1","common_dir":"/repo/.git",
				"inventory_usable":true,"primary_graph_id":"graph-1",
				"checkouts":[{"admin_name":"wt","root_path":"/repo/wt",
					"action":"ready_confirmed","detail":"the root answered"}]}]}`), nil
	})

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, runReposReconcile(cmd, nil))
	require.Empty(t, gotArgs, "no argument reconciles every family")
	out := buf.String()
	require.Contains(t, out, "family family-1  inventory_usable=true  primary=graph-1")
	require.Contains(t, out, "ready_confirmed")
	require.Contains(t, out, "1 families, 0 removed, 1 coordinators live")

	cmd, _ = newCheckoutsTestCmd(t)
	require.NoError(t, runReposReconcile(cmd, []string{"/repo/wt"}))
	require.Equal(t, map[string]any{"family": "/repo/wt"}, gotArgs)
}

func TestReposExplainViewRendersEachBinding(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name: "routed worktree",
			payload: `{"path":"/repo/wt","matched":true,"family_id":"family-1",
				"checkout_id":"c1","admin_name":"wt","root_path":"/repo/wt",
				"checkout_state":"checkout_ready","effective_mode":"automatic",
				"graph_id":"graph-1","repo_prefix":"repo","primary_graph_id":"graph-1",
				"route":{"graph_id":"graph-1","commit_generation_id":5,"dirty_generation_id":6,
					"route_epoch":3,"state":"active","ready":true},
				"coordinator_live":true,"composed":true,
				"chain":["checkout wt (c1) owns the longest root containing the path",
					"both layers are published: the composed checkout view answers"]}`,
			want: []string{
				"checkout: wt (c1)  automatic/checkout_ready",
				"route: graph-1 epoch=3 active commit=5 dirty=6 ready=true",
				"composed: true  coordinator: true",
				"- both layers are published",
			},
		},
		{
			name: "dedicated checkout",
			payload: `{"path":"/repo","matched":true,"family_id":"family-1",
				"checkout_id":"c0","admin_name":"@main","effective_mode":"dedicated",
				"checkout_state":"checkout_ready","graph_id":"graph-1","repo_prefix":"repo",
				"composed":false,
				"reason":"the checkout is dedicated, so its own corpus answers directly",
				"chain":["mode is dedicated: no layers are composed"]}`,
			want: []string{
				"checkout: @main (c0)  dedicated/checkout_ready",
				"answers from: repo  graph-1",
				"composed: false",
				"reason: the checkout is dedicated",
			},
		},
		{
			name: "unknown path",
			payload: `{"path":"/elsewhere","matched":false,"composed":false,
				"reason":"no registered checkout contains this path",
				"chain":["path /elsewhere is inside no registered checkout"]}`,
			want: []string{
				"checkout: (none)",
				"composed: false",
				"reason: no registered checkout contains this path",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs map[string]any
			stubCheckoutsTool(t, func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
				require.Equal(t, "explain_view", tool)
				gotArgs = args
				return json.RawMessage(tc.payload), nil
			})
			cmd, buf := newCheckoutsTestCmd(t)
			require.NoError(t, runReposExplainView(cmd, []string{"/target"}))
			require.Equal(t, map[string]any{"path": "/target"}, gotArgs)
			for _, want := range tc.want {
				require.Contains(t, buf.String(), want)
			}
		})
	}
}

func TestUntrackPreviewsADestructivePlanAndConfirmsOnDemand(t *testing.T) {
	orig := untrackDaemonTool
	t.Cleanup(func() { untrackDaemonTool = orig })
	t.Cleanup(func() { untrackConfirm, untrackFormat = false, "text" })

	var gotArgs map[string]any
	untrackDaemonTool = func(_ string, tool string, args map[string]any) (json.RawMessage, error) {
		require.Equal(t, "untrack_repository", tool)
		gotArgs = args
		if args["confirm"] == true {
			return json.RawMessage(`{"status":"untracked","plan":"primary_closure",
				"prefix":"repo","nodes_removed":9,"edges_removed":8}`), nil
		}
		return json.RawMessage(`{"status":"preview","action":"untrack","plan":"primary_closure",
			"prefix":"repo","is_primary":true,"sole_primary":true,"confirm_required":true,
			"detail":"nothing was written",
			"closure":[{"kind":"graph","id":"graph-1","detail":"the family's primary corpus repo is retired"}],
			"preserved":[]}`), nil
	}

	untrackConfirm, untrackFormat = false, "text"
	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo"))
	require.Equal(t, map[string]any{"path": "/repo"}, gotArgs,
		"a preview must not send a confirm")
	out := buf.String()
	require.Contains(t, out, `preview: /repo would run the "primary_closure" plan`)
	require.Contains(t, out, "primary: yes  sole=true")
	require.Contains(t, out, "removes graph: the family's primary corpus repo is retired")
	require.Contains(t, out, "run: gortex untrack /repo --confirm")

	untrackConfirm = true
	cmd, buf = newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo"))
	require.Equal(t, true, gotArgs["confirm"])
	require.Contains(t, buf.String(), "[gortex] untracked /repo (via daemon)")
}

func TestUntrackReportsADemotion(t *testing.T) {
	orig := untrackDaemonTool
	t.Cleanup(func() { untrackDaemonTool = orig })
	t.Cleanup(func() { untrackConfirm, untrackFormat = false, "text" })

	untrackConfirm, untrackFormat = false, "text"
	untrackDaemonTool = func(_ string, _ string, args map[string]any) (json.RawMessage, error) {
		require.NotContains(t, args, "confirm", "a demotion is not destructive")
		return json.RawMessage(`{"status":"demoted","plan":"demote","prefix":"repo@wt","demoted":true}`), nil
	}

	cmd, buf := newCheckoutsTestCmd(t)
	require.NoError(t, untrackViaDaemon(cmd, buf, ".", "/repo/wt"))
	require.Contains(t, buf.String(),
		"[gortex] demoted /repo/wt to its family's automatic lane (via daemon)")
}
