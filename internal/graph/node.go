package graph

import (
	"strings"
	"time"
)

// Node Meta keys shared between the extractor that writes them and the
// tier that reads them, so both sides compile against one spelling.
const (
	// MetaOwnershipStartLine / MetaOwnershipEndLine carry the 1-based line
	// span an extractor attributed a member's calls by, stamped only when
	// that span is not contained by the node's own lines: a C# 13 partial
	// property extracted declaring fragment first (or a property declared
	// in both arms of an #if / #else) keeps its first declaration's lines
	// while its calls live in the body-bearing fragment. Absent on every
	// node whose own lines already answer. Any pass that rebases a node's
	// lines (the Razor code-block delegation) must rebase these with them.
	// Read by the semantic tier's stub admission guard (issue #731).
	MetaOwnershipStartLine = "ownership_start_line"
	MetaOwnershipEndLine   = "ownership_end_line"
)

type NodeKind string

const (
	KindFile      NodeKind = "file"
	KindPackage   NodeKind = "package"
	KindFunction  NodeKind = "function"
	KindMethod    NodeKind = "method"
	KindType      NodeKind = "type"
	KindInterface NodeKind = "interface"
	KindVariable  NodeKind = "variable"
	KindImport    NodeKind = "import"
	KindContract  NodeKind = "contract"
	// KindField represents a struct field, class property, or record
	// field — anything addressable as `owner.field`. ID convention:
	// `<file>::<owner>.<field>`. EdgeMemberOf links the field to its
	// owning type. Languages that already emitted class properties as
	// KindVariable (TypeScript, PHP) keep doing so for backwards
	// compatibility — KindField is reserved for languages that
	// previously emitted only type-ref edges from fields (Go, Rust,
	// Java, C#).
	KindField NodeKind = "field"
	// Coverage kinds: each is gated behind a per-domain
	// .gortex.yaml::index.coverage.<domain>.enabled. Parsers register a
	// kind on first use; the registry is permissive (validNodeKinds
	// accepts all known kinds) so an unenabled domain simply produces no
	// nodes of that kind, rather than failing extraction.

	// KindParam represents a single function/method parameter. ID
	// convention: `<func-id>#param:<name>`. EdgeParamOf links the param
	// node back to its owner; EdgeTypedAs binds it to its declared
	// type. Created when index.function_shape.enabled is true.
	KindParam NodeKind = "param"
	// KindClosure represents an anonymous function / lambda inside an
	// enclosing function. ID convention: `<file>::<enclosing>#closure@<line>`.
	// Calls/reads/writes inside the closure attribute to the closure
	// node, not its enclosing function. EdgeMemberOf links to the
	// enclosing function. EdgeCaptures lists outer bindings closed over.
	KindClosure NodeKind = "closure"
	// KindLocal represents an intra-function binding — a variable
	// declared inside a function body via `x := …` / `var x = …` / a
	// range clause / a type-switch / a for-init clause. ID convention:
	// `<ownerID>#local:<name>@+<offsetFromOwnerStartLine>` (the
	// leading `+` flags the value as a relative offset so the IDs
	// stay stable when the enclosing function moves as a whole).
	// EdgeMemberOf links each binding to its enclosing function or
	// method. KindLocal is excluded from the BM25 search index by
	// shouldIndexForSearch — surfacing `err` / `data` / `n` / `i`
	// from every function would flood every name lookup. The data-
	// flow analysis (flow_between, taint_paths, ...) traverses the
	// EdgeValueFlow / EdgeArgOf / EdgeReturnsTo edges that target
	// these nodes; consumers that want the locals can ask for them
	// by kind explicitly.
	KindLocal NodeKind = "local"
	// KindBuiltin represents a language intrinsic — a function /
	// type / constant that's part of the language itself, not
	// declared in any indexed source file. ID convention:
	// `builtin::<lang>::<name>` for functions (`builtin::go::append`,
	// `builtin::py::len`) and `builtin::<lang>::type::<name>` for
	// types (`builtin::go::type::string`). Meta.builtin_kind ∈
	// "func" | "type" | "const". KindBuiltin is excluded from the
	// BM25 search index — surfacing `string` / `int` / `append`
	// would flood every name lookup. They participate in normal
	// graph queries: `find_usages(builtin::go::type::float64)`
	// answers "every variable typed as float64 in this codebase",
	// which is the load-bearing query for type-drift / dataflow
	// analyses.
	KindBuiltin NodeKind = "builtin"
	// KindConstant peels off `const`, `iota`, top-level immutable
	// bindings, and language-specific constant declarations from
	// KindVariable. Existing variable-kind nodes are re-classified on
	// next index; IDs are preserved.
	KindConstant NodeKind = "constant"
	// KindEnumMember represents one member of an enum-like type. ID
	// convention: `<file>::<EnumType>.<Member>`. EdgeMemberOf links to
	// the enum's type node.
	KindEnumMember NodeKind = "enum_member"
	// KindGenericParam represents a type parameter declared by a
	// function or type. ID convention: `<owner-id>#tparam:<name>`.
	KindGenericParam NodeKind = "generic_param"
	// KindModule represents a single (ecosystem, name, version) tuple
	// for an external dependency. Shared across files that import it.
	// ID convention: `module::<ecosystem>:<name>@<version>`.
	// Ecosystems: go, npm, pypi, cargo, maven, composer, gem, hex, nuget.
	KindModule NodeKind = "module"
	// KindTable represents a database table. ID convention:
	// `db::<dialect>::<schema>.<table>`. Sourced from migrations, ORM
	// models, and string-literal SQL in priority order.
	KindTable NodeKind = "table"
	// KindColumn represents a database column. ID convention:
	// `db::<dialect>::<schema>.<table>.<column>`. EdgeMemberOf links to
	// the owning table.
	KindColumn NodeKind = "column"
	// KindConfigKey represents a configuration key — env var, viper
	// path, CLI flag, struct-tag-driven field, or k8s ConfigMap entry.
	// ID convention: `cfg::<source>::<dotted.path>`. Source ∈
	// env|viper|flags|k8s_cm|k8s_secret|struct_tag.
	KindConfigKey NodeKind = "config_key"
	// KindFlag represents a feature flag / experiment. ID convention:
	// `flag::<provider>::<name>`. Provider ∈ growthbook|launchdarkly|
	// unleash|internal|env.
	KindFlag NodeKind = "flag"
	// KindEvent represents a log, metric, span, or trace name emitted
	// from code, or a pub/sub topic/channel/subject. ID convention:
	// `event::<kind>::<name>` for observability events and
	// `event::pubsub::<transport>::<name>` for pub/sub topics.
	// Meta["event_kind"] ∈ log|metric|trace|span|pubsub. For pubsub
	// events Meta["transport"] ∈ nats|kafka|rabbitmq|redis|socketio|
	// eventemitter|unknown; publishers link in via EdgeEmits and
	// subscribers via EdgeListensOn.
	KindEvent NodeKind = "event"
	// KindMigration represents a database migration unit. ID
	// convention: `migration::<dialect>::<id>`. Provides tables/columns
	// it creates; consumes ones it references.
	KindMigration NodeKind = "migration"
	// KindFixture represents a test data file or golden file. ID
	// convention: `fixture::<path>`. Test functions reference it via
	// EdgeReferences.
	KindFixture NodeKind = "fixture"
	// KindTodo represents a TODO/FIXME/HACK/XXX/NOTE comment marker. ID
	// convention: `todo::<file>:<line>`. Meta carries tag, assignee,
	// due, ticket, and the truncated text.
	KindTodo NodeKind = "todo"
	// KindTeam represents a CODEOWNERS team or individual. ID
	// convention: `team::<name>`. Meta.kind ∈ team|person disambiguates.
	KindTeam NodeKind = "team"
	// KindRelease represents a tag/version boundary. ID convention:
	// `release::<tag>`. Used as a query filter via Node.Meta["added_in"]
	// rather than as an edge endpoint in most cases.
	KindRelease NodeKind = "release"
	// KindLicense represents an SPDX license identifier. ID convention:
	// `license::<spdx>`. Files link to it via EdgeLicensedAs.
	KindLicense NodeKind = "license"
	// KindString represents a string literal that crosses an API
	// boundary worth tracking — Datadog/Prometheus metric names,
	// errors.New / fmt.Errorf messages, raw HTTP route paths, and
	// (later) HTML class/id values. ID convention:
	// `string::<context>::<value-or-hash>`. Context ∈
	// metric|error_msg|route|html_class|html_id|… EdgeEmits links the
	// enclosing function/method to the string node, mirroring KindEvent.
	// Per-repo: applyRepoPrefix prefixes every node ID with the repo
	// slug so two repos that emit the same string don't collide.
	KindString NodeKind = "string"
	// KindResource represents a Kubernetes manifest resource —
	// Deployment, Service, ConfigMap, Secret, Ingress, CronJob,
	// StatefulSet, DaemonSet, Job, ReplicaSet, ServiceAccount,
	// Role, RoleBinding, ClusterRole, ClusterRoleBinding, Namespace,
	// PersistentVolume, PersistentVolumeClaim, etc. ID convention:
	// `k8s::<kind>::<namespace>::<name>` (namespace defaults to
	// "_" when not declared in the manifest). Meta carries
	// api_version, namespace, labels (truncated). Sourced from YAML
	// extractors that detect K8s manifests by `apiVersion:` +
	// `kind:` markers.
	KindResource NodeKind = "resource"
	// KindKustomization represents a Kustomize overlay — one per
	// `kustomization.yaml` / `kustomization.yml` file in a repo.
	// ID convention: `kustomize::<dir>` where dir is the directory
	// holding the kustomization file relative to the repo root.
	// Resources, bases, components, and patches are linked via
	// EdgeDependsOn (overlay → base) and EdgeReferences
	// (overlay → resource files).
	KindKustomization NodeKind = "kustomization"
	// KindImage represents either a container image or a raster/vector
	// image asset — distinguished by ID prefix and Meta. Container images
	// come from a Dockerfile FROM (external base or `FROM ... AS <stage>`)
	// or a K8s container spec; image assets are picture files ingested by
	// the multimodal extractor. ID conventions:
	//   `image::<name>:<tag>` for external/registry images (tag
	//     defaults to "latest" when omitted)
	//   `image::stage::<file>::<stage-name>` for Dockerfile build
	//     stages
	//   `image::asset::<path>` for ingested image-file assets
	// Meta carries registry/digest/platform for container images, and
	// format/width/height/size_bytes/sha256 (asset_kind="image") for
	// image-file assets.
	KindImage NodeKind = "image"
	// KindArtifact represents a non-code knowledge file declared in
	// the `.gortex.yaml::artifacts` manifest — a DB schema (SQL /
	// Prisma / dbt), an API spec (OpenAPI / GraphQL / protobuf), an
	// infra config (Terraform / Kustomize / Helm), or an
	// architecture doc (ADR markdown). ID convention:
	// `artifact::<repo-relative-path>`. Meta carries artifact_kind
	// (schema|api|infra|doc), content_hash (sha256 of the file —
	// drives staleness detection), title, and size. The artifact
	// node links to every symbol it mentions via EdgeReferences so
	// agents can pull the right schema or spec alongside the code.
	KindArtifact NodeKind = "artifact"
	// KindDoc represents one heading-delimited prose section of a
	// documentation file (Markdown). Name is the breadcrumb heading
	// path ("README.md > Setup > Build"); Meta["section_text"] holds
	// the section's paragraph text with markdown syntax stripped, and
	// the BM25 search index is fed that body so a prose query ranks
	// the right section. ID convention:
	// "<file>::doc:<slug-of-heading-path>" -- derived from the
	// heading path, NOT line numbers, so an incremental reindex of an
	// edited file keeps stable section identity. The owning file
	// links to it via EdgeDefines.
	KindDoc NodeKind = "doc"
	// KindRationale is a graph projection of a development-memory record
	// (a decision / incident / constraint / invariant) — the "why" behind
	// code. The store_memory sidecar stays the system of record; this node
	// is a derived view re-projected on memory write and reconciled on
	// warmup, so a why-query is one hop from the code it explains. ID
	// convention: "rationale::<memory-id>". Links to the code it explains
	// via EdgeMotivates.
	KindRationale NodeKind = "rationale"
	// KindTopic represents a message-broker topic / subject / channel /
	// exchange — the contract-layer pairing artefact for Kafka,
	// RabbitMQ, NATS, and Redis pub-sub. ID convention:
	// `topic::<broker>::<name>` where broker ∈ kafka|rabbitmq|nats|
	// redis. Meta carries broker (the family) and name (the raw topic
	// / subject / channel / exchange string). Producer symbols link
	// in via EdgeProducesTopic, consumers via EdgeConsumesTopic, so a
	// cross-service event flow is a two-hop path
	// producer --produces_topic--> topic <--consumes_topic-- consumer.
	// KindTopic is distinct from KindEvent (which the observability /
	// pubsub extractor uses for the same calls): KindEvent rides at
	// the call-site evidence tier (heuristic / inferred) and is for
	// broad "what publishes here" queries, while KindTopic is the
	// pairing artefact produced by the contracts matcher and rides at
	// the structural ast_resolved tier.
	KindTopic NodeKind = "topic"
	// KindMacro represents a C/C++ preprocessor macro defined with
	// #define — both object-like (`#define PI 3.14`) and function-like
	// (`#define SQ(x) ((x)*(x))`). ID convention: `<file>::<NAME>`.
	// Meta["macro_kind"] ∈ object|function; function-like macros carry
	// Meta["params"] (the parameter names) and Meta["replacement"] (the
	// replacement-list text). The owning file links via EdgeDefines, and
	// a function-like macro emits EdgeCalls to the symbols its
	// replacement list invokes — recovering call edges that would
	// otherwise be hidden behind macro expansion (a call site `SQ(2)`
	// resolves to the macro, whose body-calls then continue the chain).
	KindMacro NodeKind = "macro"
	// KindAgent represents a live coding agent participating in a
	// multi-agent session — a first-class, queryable presence entity
	// distinct from a transport session. ID convention: `agent::<id>`.
	// Carries Meta["cursor"] (the symbol/file the agent is focused on),
	// Meta["status"], Meta["locked_paths"], and Meta["last_seen"]. Agent
	// presence is volatile and lives in an in-memory registry rather than
	// the persistent code graph; this kind names the entity so it reads as
	// first-class in tool output and query filters.
	KindAgent NodeKind = "agent"
	// KindContractBridge represents one matched provider↔consumer
	// contract group — an HTTP route, a gRPC/Thrift method, or a
	// pub/sub topic — materialised as a single graph node that spans
	// every repo participating in the group. ID convention:
	// `bridge::<contract-id>` where contract-id is the canonical
	// contract key (`http::GET::/v1/users`, `grpc::Users::GetUser`,
	// `topic::kafka::orders`), so the bridge for any contract is
	// addressable from the contract ID alone, across repos. Meta
	// carries contract_type, canonical_key, repos (sorted slice of
	// participating repo prefixes), provider_count, consumer_count
	// and cross_repo. EdgeBridges links the bridge to each
	// participating KindContract node. Bridge nodes are re-derived
	// from the matcher result on every contract reconcile — all of
	// them share the synthetic FilePath "contracts://bridges" so the
	// reconcile pass can evict the stale generation with one
	// EvictFile call before re-minting.
	KindContractBridge NodeKind = "contract_bridge"
)

// IsValidNodeKind reports whether s names a known node kind. Used by
// the search layer to tell a real node-kind clause apart from a
// flavor value that only looks like a kind (e.g. codegraph's
// `kind:class`).
func IsValidNodeKind(s string) bool {
	return validNodeKinds[NodeKind(s)]
}

var validNodeKinds = map[NodeKind]bool{
	KindFile: true, KindPackage: true, KindFunction: true,
	KindMethod: true, KindType: true, KindInterface: true,
	KindVariable: true, KindImport: true, KindContract: true,
	KindField: true,
	// Coverage kinds — see Kind* doc comments above for usage notes.
	KindParam: true, KindClosure: true, KindConstant: true,
	KindEnumMember: true, KindGenericParam: true, KindModule: true,
	KindTable: true, KindColumn: true, KindConfigKey: true,
	KindFlag: true, KindEvent: true, KindMigration: true,
	KindFixture: true, KindTodo: true, KindTeam: true,
	KindRelease: true, KindLicense: true, KindString: true,
	KindResource: true, KindKustomization: true, KindImage: true,
	KindArtifact: true, KindDoc: true, KindTopic: true,
	KindRationale: true,
	KindMacro:     true, KindAgent: true, KindContractBridge: true,
}

type Node struct {
	ID        string   `json:"id"`
	Kind      NodeKind `json:"kind"`
	Name      string   `json:"name"`
	QualName  string   `json:"qual_name,omitempty"`
	FilePath  string   `json:"file_path"`
	StartLine int      `json:"start_line"`
	// EndLine is omitted when zero — File-kind nodes don't have ranges.
	EndLine int `json:"end_line,omitempty"`
	// StartColumn / EndColumn are 0-based source column offsets of the
	// symbol's span. Omitted when zero (most extractors record only line
	// ranges); promoted to typed nodes columns on the SQLite backend.
	StartColumn int            `json:"start_column,omitempty"`
	EndColumn   int            `json:"end_column,omitempty"`
	Language    string         `json:"language"`
	Meta       map[string]any `json:"meta,omitempty"`
	RepoPrefix string         `json:"repo_prefix,omitempty"`
	// WorkspaceID is the hard graph boundary slug. Two nodes with
	// different WorkspaceIDs are not allowed to be matched as contract
	// provider/consumer pairs and queries scope by it by default.
	// Defaults at warmup time to the per-repo `.gortex.yaml::workspace`
	// setting; falls back to RepoPrefix when no workspace is declared
	// (so old configs keep working) and to "" only for snapshot
	// records written before the field existed (gob decodes unknown
	// fields as zero — warmup backfills these from config).
	WorkspaceID string `json:"workspace_id,omitempty"`
	// ProjectID is the soft sub-boundary inside a workspace. One
	// project per repo by default; monorepos can declare projects[] in
	// .gortex.yaml. Contract pairing is bounded to a single
	// (workspace_id, project_id); cross-project contracts become orphans.
	// Defaults to the repo name when no projects[] mapping matches.
	ProjectID string `json:"project_id,omitempty"`
	// AbsoluteFilePath is the on-disk absolute path corresponding to
	// FilePath. It is empty on the canonical graph node and is populated
	// only on the per-response copies the MCP layer hands to result
	// encoders, so an editor or agent can open a result directly without
	// reconstructing the path from repo_prefix + file_path.
	AbsoluteFilePath string `json:"absolute_file_path,omitempty"`

	// Origin marks a node minted by the cross-daemon proxy-edge feature
	// as standing in for a symbol another daemon owns. "" on every
	// locally-indexed node; "remote:<slug>" on a proxy node. Written ONLY
	// by the proxy-edge mint path; the read-only fan-out carries
	// provenance in the response, never here. Excluded from
	// graph_stats / BM25 / communities / analyzers (see IsProxyNode).
	Origin string `json:"origin,omitempty"`
	// Stub marks a node as a federation proxy placeholder whose
	// neighbour edges hydrate lazily over /v1/subgraph rather than from
	// local extraction. Always true together with a non-empty Origin.
	Stub bool `json:"stub,omitempty"`
	// FetchedAt is the wall-clock time the proxy node (and its hydrated
	// ring) was last pulled from the remote — drives the TTL freshness
	// gate and the federated last_synced field. Zero on local nodes.
	FetchedAt time.Time `json:"fetched_at,omitempty"`
}

// IsReExportNode reports whether n is a barrel re-export binding — a node
// minted at an `export { X } from './mod'` site that forwards another module's
// declaration under the exported (post-alias) name. Marked with
// Meta["reexport"]==true by the JS/TS extractor. These nodes are transparent
// aliases: call resolution skips them (a call binds to the forwarded
// declaration, not the façade), while find_usages delegates their usage set to
// the canonical target.
func IsReExportNode(n *Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, ok := n.Meta["reexport"].(bool)
	return ok && v
}

// Brief returns a compact representation with only the fields needed for listing.
func (n *Node) Brief() map[string]any {
	b := map[string]any{
		"id":         n.ID,
		"name":       n.Name,
		"kind":       n.Kind,
		"file_path":  n.FilePath,
		"start_line": n.StartLine,
	}
	if n.RepoPrefix != "" {
		b["repo_prefix"] = n.RepoPrefix
	}
	if n.WorkspaceID != "" {
		b["workspace_id"] = n.WorkspaceID
	}
	if n.ProjectID != "" {
		b["project_id"] = n.ProjectID
	}
	// Surface visibility and a short doc snippet when present — Brief
	// is the listing projection used by search_symbols and find_usages,
	// where these two fields meaningfully sharpen the result so the
	// agent can decide without a follow-up get_symbol_source call.
	if v, ok := n.Meta["visibility"].(string); ok && v != "" {
		b["visibility"] = v
	}
	if d, ok := n.Meta["doc"].(string); ok && d != "" {
		// Truncate doc to 80 chars in Brief — the full doc is on the
		// node, this is just the listing teaser.
		const briefDocCap = 80
		if len(d) > briefDocCap {
			d = d[:briefDocCap] + "…"
		}
		b["doc"] = d
	}
	// Test classification — stamped by the indexer's test-edge pass.
	// Surfacing it on the listing row lets agents tell production
	// callers from test callers without a follow-up call.
	if v, ok := n.Meta["is_test"].(bool); ok && v {
		b["is_test"] = true
	}
	if r, ok := n.Meta["test_role"].(string); ok && r != "" {
		b["test_role"] = r
	}
	if r, ok := n.Meta["test_runner"].(string); ok && r != "" {
		b["test_runner"] = r
	}
	if v, ok := n.Meta["is_test_file"].(bool); ok && v {
		b["is_test_file"] = true
	}
	// Structural flavor + UI-component framework — stamped by the
	// language extractors. Surfacing them on the listing row lets an
	// agent filter / triage by shape (struct vs class, react vs svelte)
	// without a follow-up call.
	if v, ok := n.Meta["type_flavor"].(string); ok && v != "" {
		b["type_flavor"] = v
	}
	if v, ok := n.Meta["ui_component"].(string); ok && v != "" {
		b["ui_component"] = v
	}
	// A prose-section node carries no signature -- surface a short
	// snippet of its body text so a docs search result is
	// self-describing without a follow-up read.
	if n.Kind == KindDoc {
		if txt, ok := n.Meta["section_text"].(string); ok && txt != "" {
			const snippetCap = 160
			if len(txt) > snippetCap {
				txt = txt[:snippetCap] + "\u2026"
			}
			b["section"] = txt
		}
	}
	// enclosing / enclosing_id name the symbol this node is declared
	// inside -- the receiver type of a method, the struct of a field,
	// the enum of a member, the function around a closure. Derived
	// from the ID convention; absent for top-level symbols. Lets a
	// search result say "Parse on type Decoder" without a follow-up
	// call.
	if eid, ename := EnclosingFromID(n.ID, n.Kind); ename != "" {
		b["enclosing"] = ename
		b["enclosing_id"] = eid
	}
	// AbsoluteFilePath is populated only on the per-response copies the
	// MCP layer builds (see Server.withAbsPaths); empty on canonical nodes.
	if n.AbsoluteFilePath != "" {
		b["absolute_file_path"] = n.AbsoluteFilePath
	}
	return b
}

// EnclosingFromID derives a node's enclosing owner purely from its
// ID and kind -- no graph access. It covers the kinds whose ID
// convention embeds the owner:
//
//   - method  "<file>::<Owner>.<method>"      -> owner "<file>::<Owner>"
//   - field   "<file>::<owner>.<field>"       -> owner "<file>::<owner>"
//   - enum    "<file>::<EnumType>.<Member>"   -> owner "<file>::<EnumType>"
//   - closure "<file>::<enclosing>#closure@N" -> owner "<file>::<enclosing>"
//
// For every other kind -- and for a method/field/closure whose ID
// carries no owner segment -- both return values are empty. The
// returned name is the owner's short (last-segment) name.
//
// This is the standalone derivation Node.Brief uses; callers with a
// graph reader should prefer the richer EdgeMemberOf-based lookup,
// which also resolves owners the ID does not name.
func EnclosingFromID(id string, kind NodeKind) (ownerID, ownerName string) {
	sep := strings.Index(id, "::")
	if sep < 0 {
		return "", ""
	}
	file, symbol := id[:sep], id[sep+2:]
	switch kind {
	case KindClosure:
		// "<enclosing>#closure@<line>" -- the owner is the segment
		// before the first '#'.
		if h := strings.IndexByte(symbol, '#'); h > 0 {
			owner := symbol[:h]
			return file + "::" + owner, lastIDSegment(owner)
		}
		return "", ""
	case KindMethod, KindField, KindEnumMember:
		// "<Owner>.<member>" -- the owner is everything before the
		// last '.'.
		if dot := strings.LastIndexByte(symbol, '.'); dot > 0 {
			owner := symbol[:dot]
			return file + "::" + owner, lastIDSegment(owner)
		}
		return "", ""
	default:
		return "", ""
	}
}

// lastIDSegment returns the last dotted segment of an identifier --
// its human-facing short name.
func lastIDSegment(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// EnclosingShortName returns the human-facing short name of an
// owner identifier or node ID -- its last "::"- or "."-separated
// segment. Used when only an owner ID string is in hand and no node
// was resolved.
func EnclosingShortName(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	return lastIDSegment(s)
}

func ValidNodeKind(k NodeKind) bool {
	return validNodeKinds[k]
}
