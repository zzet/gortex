package graph

type EdgeKind string

const (
	EdgeImports EdgeKind = "imports"
	// EdgeReExports links a file to a binding it forwards from another
	// module — the barrel-file pattern `export { x } from "mod"` /
	// `export * from "mod"` / `export * as ns from "mod"`. Direction:
	// file → unresolved::import::<path>[::<original>]. Distinct from
	// EdgeImports so a dependency walk can tell "this file re-exports
	// mod's surface" (a transitive forwarding hop) from "this file
	// consumes mod". The renamed name rides on Edge.Alias.
	EdgeReExports EdgeKind = "re_exports"
	// EdgeContains links a file node to its non-symbol children — import
	// nodes today, and a natural home for future side-band kinds
	// (todos, fixtures) that "belong to" a file without being defined
	// by it. EdgeDefines is the wrong fit for these because the file
	// does not semantically *define* an import; it *contains* the
	// import statement. Splitting the kinds lets walkers that want
	// "real definitions" follow EdgeDefines and walkers that want the
	// full file neighbourhood union both. The disk-backed
	// GetFileSubGraph relies on this union to fetch every file
	// neighbour in one pass.
	EdgeContains     EdgeKind = "contains"
	EdgeDefines      EdgeKind = "defines"
	EdgeCalls        EdgeKind = "calls"
	EdgeInstantiates EdgeKind = "instantiates"
	EdgeImplements   EdgeKind = "implements"
	EdgeExtends      EdgeKind = "extends"
	EdgeReferences   EdgeKind = "references"
	// EdgeMotivates links knowledge — a KindRationale, KindArtifact, or
	// content KindDoc — to the code symbol it explains or justifies: the
	// causal "why this exists" relation, distinct from the structural
	// mention EdgeReferences. Source: rationale projection and the
	// doc->code linking pass.
	EdgeMotivates EdgeKind = "motivates"
	EdgeMemberOf  EdgeKind = "member_of"
	EdgeProvides  EdgeKind = "provides"
	EdgeConsumes  EdgeKind = "consumes"
	// EdgeMatches links a consumer contract node to the provider contract
	// node it resolves to (e.g. consumer http:GET:/v1/tucks → provider
	// http:GET:/v1/tucks, across repos). Traversals bridge service
	// boundaries by hopping Consumer → EdgeConsumes⁻¹ → consumer-contract
	// → EdgeMatches → provider-contract → EdgeProvides⁻¹ → handler.
	EdgeMatches EdgeKind = "matches"
	// EdgeBridges links a KindContractBridge group node to one of the
	// KindContract nodes participating in the group. Direction:
	// bridge → contract. Meta["side"] ∈ provider | consumer | both
	// ("both" when the provider and consumer contracts share one ID
	// and therefore collapse into a single contract node in the
	// graph). Emitted by the contract-bridge materialisation pass in
	// ReconcileContractEdges alongside EdgeMatches; the whole bridge
	// generation is evicted and re-derived on every reconcile so the
	// edges never outlive the contracts they group.
	EdgeBridges EdgeKind = "bridges"
	// EdgeAnnotated links a symbol to a synthetic annotation node
	// representing a decorator / annotation / attribute applied to it
	// (e.g. @Component, @Test, @Deprecated, #[derive(Debug)],
	// [Authorize], @app.route("/x"), @Published). The annotation node's
	// ID follows the convention "annotation::<lang>::<name>"; the edge's
	// Meta["args"] carries the verbatim argument text (truncated) when
	// the annotation has parentheses.
	//
	// Framework dispatch (NestJS @Get, Laravel middleware, Symfony
	// AsEventListener, Spring @Bean, FastAPI @app.route, …) continues
	// to flow through the contracts/dispatch layer with
	// EdgeProvides/EdgeConsumes — EdgeAnnotated runs in parallel as a
	// queryable record of the raw decorator. This lets agents answer
	// "find all @Deprecated" / "find all controllers" with one graph
	// hop without duplicating contract logic.
	EdgeAnnotated EdgeKind = "annotated"
	// EdgeTests links a test function/method to a non-test symbol it
	// exercises. Computed at index time as a post-extraction pass:
	// every call edge whose source is a test function (Meta["is_test"]
	// = true) and whose target is non-test produces an EdgeTests pair
	// alongside the existing EdgeCalls. Lets agents answer
	// "which tests cover X" with a single reverse-edge walk and lets
	// `get_untested_symbols` filter public symbols whose inverse-EdgeTests
	// set is empty.
	//
	// Test detection is by file naming convention plus per-language
	// fn-name conventions (Test*/Benchmark*/Fuzz* in Go, test_* /
	// Test* in Python, *_test.dart, etc.). Override per-repo via
	// .gortex.yaml::test_patterns when the project uses an unusual
	// layout — false positives are an acknowledged tradeoff for
	// keeping the heuristic dependency-free.
	EdgeTests EdgeKind = "tests"
	// EdgeReads / EdgeWrites split EdgeReferences for value-side uses
	// of variables and fields. LHS of an assignment / op= / ++ / --
	// emits EdgeWrites; every other identifier or selector use emits
	// EdgeReads. EdgeReferences is reserved for type references
	// (`var x SomeType` references the type SomeType) so the resolver
	// can keep distinguishing the two by target node kind.
	//
	// Together with KindField, these let agents ask "which functions
	// write to this field" — impossible with the previous "any use is
	// a reference" model. Implemented per-language as the Go/TS/
	// Python (priority wave) and Rust/Java (second wave) extractors
	// learn to walk assignment AST nodes.
	EdgeReads  EdgeKind = "reads"
	EdgeWrites EdgeKind = "writes"
	// EdgeThrows links a function/method to an error or exception
	// type that can propagate from it. Per language:
	//
	//   go      function returns an error type → edge to that type
	//           (custom *MyError type or external::error sentinel for
	//           the built-in error interface).
	//   python  `raise <Exception>` AST nodes inside the body.
	//   java    method `throws` clause.
	//   swift   `throws` / `rethrows` keyword on the function decl.
	//   rust    return type contains Result<_, E> → edge to E.
	//
	// Lets agents ask "what error types can propagate from here" with
	// a single forward walk and lets `analyze kind: "error_surface"`
	// summarise every public function's error contract without
	// re-deriving it from source.
	EdgeThrows EdgeKind = "throws"
	// Coverage edges: each is produced only when the relevant
	// index.coverage.<domain>.enabled gate is set; the registry is
	// permissive (DefaultOriginFor handles unknown kinds via the
	// confidence-score fallback).

	// EdgeParamOf links a KindParam node to its owning function or
	// method. Distinct from EdgeMemberOf (which is for fields of
	// types). Always ast_resolved by construction.
	EdgeParamOf EdgeKind = "param_of"
	// EdgeReturns links a function/method to a type it returns. Multi-
	// return Go functions emit one edge per result. Confidence reflects
	// the resolver: ast_inferred when the type is named in source,
	// promoted to ast_resolved / lsp_resolved by the semantic layer.
	EdgeReturns EdgeKind = "returns"
	// EdgeTypedAs binds a variable, parameter, field, or constant to
	// its declared type. Lets traversals answer "find all values of
	// type T". Distinct from EdgeReferences, which is broader.
	EdgeTypedAs EdgeKind = "typed_as"
	// EdgeCaptures links a closure node to an outer binding it closes
	// over.
	EdgeCaptures EdgeKind = "captures"
	// EdgeSpawns links a caller to a function it launches
	// asynchronously (goroutine, async/await, Promise, worker pool,
	// background scheduler). Emitted in addition to the corresponding
	// EdgeCalls so synchronous reachability queries can scope by edge
	// kind. Meta["mode"] ∈ goroutine|async|promise|worker_pool|task|
	// thread|parallel|background.
	EdgeSpawns EdgeKind = "spawns"
	// EdgeSends / EdgeRecvs link a function to a channel-typed
	// variable for channel I/O. The channel's element type is reachable
	// via the variable's EdgeTypedAs edge.
	EdgeSends EdgeKind = "sends"
	EdgeRecvs EdgeKind = "recvs"
	// EdgeQueries links a function to a database table it queries
	// against. Default origin text_matched from string-literal SQL;
	// promoted to ast_resolved when an ORM mapping is recognized.
	EdgeQueries EdgeKind = "queries"
	// EdgeReadsCol / EdgeWritesCol provide column-level resolution
	// when the SQL parser can extract it. Falls back to table-level
	// EdgeQueries when columns can't be resolved.
	EdgeReadsCol  EdgeKind = "reads_col"
	EdgeWritesCol EdgeKind = "writes_col"
	// EdgeReadsConfig / EdgeWritesConfig link a function to a config
	// key it reads or writes (env var, viper key, k8s configmap entry,
	// struct-tag binding).
	EdgeReadsConfig  EdgeKind = "reads_config"
	EdgeWritesConfig EdgeKind = "writes_config"
	// Capability edges — first-class, traversable typed edges for the
	// supply-chain / least-privilege questions "what reads the
	// environment", "what shells out", "what touches this field". They
	// are synthesised after resolution (see synthesizeCapabilityEdges)
	// from edges the language extractors already emit, so they stay in
	// sync without per-language work, while giving a single edge kind to
	// walk instead of joining through the config / dataflow layers.
	//
	// EdgeReadsEnv links a function/method to the environment variable it
	// reads. The target is the typed env-var node (KindConfigKey, id
	// cfg::env::<NAME>) shared with infra EdgeUsesEnv declarations, so
	// "which code reads $AWS_SECRET" is one reverse walk. Emitted parallel
	// to every EdgeReadsConfig whose target is a cfg::env:: key.
	EdgeReadsEnv EdgeKind = "reads_env"
	// EdgeExecutesProcess links a function/method to a process it spawns
	// (os/exec, subprocess, child_process, system, Open3, Command::new,
	// Runtime.exec, …). The target is a typed process node (KindString,
	// id string::process::<mechanism>) so an audit can enumerate every
	// function that shells out. Synthesised from EdgeCalls whose callee is
	// a known process-execution API.
	EdgeExecutesProcess EdgeKind = "executes_process"
	// EdgeAccessesField links a function/method to a struct field / class
	// property it reads or writes — the union of EdgeReads and EdgeWrites
	// restricted to KindField targets, as one traversable capability edge.
	// Meta["access"] ∈ read|write. Synthesised from EdgeReads / EdgeWrites
	// edges that target a KindField node.
	EdgeAccessesField EdgeKind = "accesses_field"
	// EdgeTogglesFlag links a function to a feature flag it checks or
	// toggles. Meta["op"] ∈ read|write|register.
	EdgeTogglesFlag EdgeKind = "toggles_flag"
	// EdgeEmits links a function to a log/metric/trace event it emits.
	// Meta carries level (for logs), unit (for metrics), and label keys.
	//
	// EdgeEmits is also the publish side of the event pub/sub layer:
	// when a function publishes to a message broker (NATS / Kafka /
	// RabbitMQ / Redis pub-sub) or an in-process EventEmitter / Socket.IO
	// channel, the edge targets a KindEvent node with
	// Meta["event_kind"]="pubsub". The subscribe side is EdgeListensOn.
	// Meta["transport"] (nats|kafka|rabbitmq|redis|socketio|eventemitter|
	// unknown) and Meta["method"] (the matched call name) ride on the
	// edge so `analyze kind=pubsub` can group by broker without
	// re-deriving it.
	EdgeEmits EdgeKind = "emits"
	// EdgeListensOn links a subscriber/consumer function to the
	// KindEvent topic node it listens on — the read side of the event
	// pub/sub layer that parallels EdgeEmits' publish side. Emitted for
	// message-broker subscriptions (NATS Subscribe / Kafka consumer
	// subscribe / RabbitMQ Consume / Redis (P)Subscribe) and in-process
	// listener registration (EventEmitter.on / Socket.IO socket.on).
	// The target node always carries Meta["event_kind"]="pubsub";
	// Meta["transport"] and Meta["method"] ride on the edge. Origin:
	// ast_inferred — detection is a method-name + string-literal-topic
	// heuristic, not a type-checked fact, so it shares the tier the
	// observability extractor uses for EdgeEmits.
	EdgeListensOn EdgeKind = "listens_on"
	// EdgeGeneratedBy links a generated file to its schema source
	// (.proto, .graphql, openapi.yaml, etc.). Detected via comment
	// markers (// Code generated …), conventional adjacency, or
	// go:generate directives.
	EdgeGeneratedBy EdgeKind = "generated_by"
	// EdgeDependsOnModule links a file/package/import to a KindModule
	// node. One edge per import statement; aggregable to package-level.
	EdgeDependsOnModule EdgeKind = "depends_on_module"
	// EdgePackageWorkspaceMember links a package-manager workspace root
	// to one of its member packages. A package-manager workspace is a
	// root manifest that owns a set of member packages — an npm/yarn
	// root package.json with a "workspaces" array, a pnpm
	// pnpm-workspace.yaml with a `packages:` list, or a Cargo root
	// Cargo.toml with `[workspace] members`. The From endpoint is the
	// synthetic workspace-root node (`pkgws::<ecosystem>:<root-dir>`);
	// the To endpoint is the member package's manifest file node.
	// Glob member patterns (`packages/*`) are expanded against the
	// filesystem at index time, so one edge is emitted per resolved
	// member directory.
	//
	// This is the package-manager's notion of a workspace and is
	// unrelated to Node.WorkspaceID, which is Gortex's hard graph
	// boundary for grouping whole repositories. Origin: ast_resolved —
	// membership is read structurally from the manifest text.
	EdgePackageWorkspaceMember EdgeKind = "package_workspace_member"
	// EdgeOwns links a team to a file or directory. Sourced from
	// CODEOWNERS. Directory entries materialize per-file.
	EdgeOwns EdgeKind = "owns"
	// EdgeAuthored links a person/team to a node they last touched.
	// Meta carries commit and timestamp. People are stored as
	// KindTeam nodes with Meta["kind"]="person".
	EdgeAuthored EdgeKind = "authored"
	// EdgeCoveredBy links a function/method to a test that exercises
	// it, with coverage_pct attached in Meta. Directional inverse of
	// EdgeTests, distinguished by carrying the coverage metric.
	EdgeCoveredBy EdgeKind = "covered_by"
	// EdgeAliases links a type alias `type X = Y` to its underlying
	// type. Distinct from EdgeExtends (`type X Y` newtype) — agents
	// distinguish by edge kind to compute correct blast radius.
	EdgeAliases EdgeKind = "aliases"
	// EdgeComposes links a type to an embedded/composed/mixed-in type
	// (Go struct embedding, Rust trait bounds, Python multiple
	// inheritance). Distinct from EdgeExtends (newtype/inheritance/
	// interface extension).
	EdgeComposes EdgeKind = "composes"
	// EdgeOverrides links a method to the parent-class or interface
	// method it overrides. Distinct from EdgeImplements (interface
	// implementation) and EdgeExtends (class hierarchy) — those are
	// type-level relationships; EdgeOverrides is method-level. Emitted
	// alongside EdgeExtends/EdgeImplements when a child type declares
	// a method that shadows a parent method with the same signature.
	// Origin tier:
	//   lsp_resolved when the LSP server confirmed the override (e.g.
	//   tsserver / rust-analyzer / clangd report it via type hierarchy
	//   or its workspace symbol provider).
	//   ast_resolved when the parent type is in the same compilation
	//   unit and the indexer can prove the method exists in both.
	//   ast_inferred when the override is heuristic (same name only,
	//   parent type unknown).
	EdgeOverrides EdgeKind = "overrides"
	// EdgeLicensedAs links a file to its SPDX license. Sourced from
	// the file's SPDX-License-Identifier header, falling back to the
	// repo-level LICENSE file.
	EdgeLicensedAs EdgeKind = "licensed_as"
	// CPG-lite dataflow primitives. Together they form the data-
	// dependence layer Gortex layers on top of the call graph: agents
	// can answer "where does this value flow?" with a single graph
	// walk instead of hand-tracing source.
	//
	// Local-binding ID convention. For language extractors that emit
	// dataflow without materialising a graph node per local variable,
	// edges target a synthetic ID of the form:
	//
	//   <ownerID>#local:<name>@+<offsetFromOwnerStartLine>
	//
	// where ownerID is the enclosing function/method/closure node
	// and the offset is the local's 1-based line minus the owner's
	// declaration line (leading `+` flags the value as a relative
	// offset). The offset-based ID keeps locals stable across edits
	// that shift the function as a whole — only edits inside the
	// function above a binding shift that binding's ID. Each ID is
	// also materialised as a KindLocal node linked to the owner
	// via EdgeMemberOf; the search index excludes KindLocal so
	// these per-binding nodes don't pollute name lookups.
	// These IDs are valid edge endpoints — BFS traverses them — but
	// no graph node is created, keeping search results free of
	// every transient binding in every function body.
	//
	// EdgeValueFlow links a value-producing position to a value-
	// consuming position within the same function/method/closure
	// body. Captures intra-procedural data-dependence: assignment
	// LHS↔RHS, range source↔induction var, return value↔function
	// symbol. Both endpoints are inside the same enclosing
	// function so the edge is fully resolved at extraction time.
	// Origin: ast_resolved by construction.
	// EdgeHandlesRoute links a handler function/method to the
	// KindContract node that represents its route. Emitted alongside
	// EdgeProvides whenever the contract type is HTTP/gRPC/WS/GraphQL/
	// topic — the framework layer that an agent asks "which symbol
	// serves /v1/users/:id?" about. The narrower edge kind lets
	// `analyze kind=routes` walk it without pulling in the broader
	// EdgeProvides graph (which also covers env keys, OpenAPI specs,
	// migrations, DI tokens, …). Origin tier mirrors the underlying
	// extractor; defaults to ast_resolved (structural by construction).
	EdgeHandlesRoute EdgeKind = "handles_route"
	// EdgeProducesTopic links a producer function/method to the
	// KindTopic node it publishes onto — the framework-layer message-
	// broker analogue of EdgeHandlesRoute for HTTP. Emitted by the
	// contracts pairing pass whenever a topic contract (Kafka /
	// RabbitMQ / NATS / Redis pub-sub) matches at least one consumer
	// across the same workspace; unpaired producers remain as
	// orphan ContractTopic records. The narrower edge kind lets
	// `analyze kind=event_emitters` and architecture queries walk
	// the producer→topic surface without pulling in the broader
	// EdgeProvides graph. The target KindTopic node carries
	// Meta["broker"] and Meta["name"] so traversals don't need to
	// re-parse the synthetic node ID. Origin: ast_resolved —
	// structural by construction once the matcher has paired the
	// underlying topic contract.
	EdgeProducesTopic EdgeKind = "produces_topic"
	// EdgeConsumesTopic is the read side of EdgeProducesTopic —
	// links a consumer function/method to the KindTopic node it
	// subscribes to. Same broker families, same pairing pass, same
	// ast_resolved tier. Multi-consumer fanout falls out naturally:
	// one producer paired with N consumers on the same (broker,
	// topic) bucket yields N EdgeConsumesTopic edges plus one
	// EdgeProducesTopic edge, all sharing the same topic node.
	EdgeConsumesTopic EdgeKind = "consumes_topic"
	// EdgeModelsTable links an ORM model type/class to the KindTable
	// node it persists. Per language:
	//
	//   go      struct with `gorm:"..."` tags or a `TableName() string`
	//           method on the receiver type
	//   python  SQLAlchemy / Django class with __tablename__ /
	//           class Meta: db_table
	//   ruby    class X < ApplicationRecord (or ActiveRecord::Base),
	//           with optional self.table_name = "..."
	//   java    @Entity class, optional @Table(name="...")
	//   typescript  TypeORM @Entity({ name: "..." }) class
	//
	// Lets agents ask "which table does this class write?" and "which
	// model owns the users table?" with a single graph hop instead of
	// joining through migrations and raw SQL.
	EdgeModelsTable EdgeKind = "models_table"
	// EdgeRendersChild links a parent component (function / method /
	// class) to a child component it renders inside its JSX/TSX/Vue/
	// Svelte template body. Captures the component dependency tree so
	// agents can ask "what renders <DataTable />?" or "what does
	// <CheckoutPage /> reach into?" without grepping for the
	// component name.
	//
	// Detection is heuristic: capital-first-letter element names
	// inside JSX expressions are treated as component references and
	// resolved through normal name resolution. Lowercase names map to
	// HTML/SVG primitives and are skipped — the edge graph would be
	// noise otherwise.
	EdgeRendersChild EdgeKind = "renders_child"
	EdgeValueFlow    EdgeKind = "value_flow"
	// EdgeArgOf links an argument expression at a call site to the
	// callee's parameter — the inter-procedural binding produced
	// by passing a value across a function boundary. Direction:
	// caller-side argument source → callee parameter. The
	// resolver lifts the unresolved callee target the same way
	// EdgeCalls is lifted; a follow-up indexer pass rewrites the
	// edge target from the callee function ID to the param node ID
	// at the recorded position (Meta["arg_position"]).
	EdgeArgOf EdgeKind = "arg_of"
	// EdgeReturnsTo links a callee function/method to the receiving
	// binding at a call site (`x := f(...)` produces returns_to(f, x)).
	// Direction: callee → assignment LHS. Stored at extraction time
	// with From = enclosing-caller ID and Meta["returns_to_call"] +
	// Meta["call_line"] as a placeholder; a follow-up indexer pass
	// rewrites From to the resolved callee ID by joining against the
	// EdgeCalls edge from the same caller+line.
	EdgeReturnsTo EdgeKind = "returns_to"
	// Infrastructure-graph edges. Materialised by the K8s
	// manifest, Kustomize, and Dockerfile extractors.
	//
	// EdgeConfigures links a workload Resource (Pod / Deployment /
	// StatefulSet / DaemonSet / Job / CronJob) to a ConfigMap or
	// Secret it pulls configuration from via `envFrom:`,
	// `valueFrom: configMapKeyRef`, or `valueFrom: secretKeyRef`.
	// Direction: consumer → provider (workload → ConfigMap/Secret).
	// Origin: ast_resolved by construction.
	EdgeConfigures EdgeKind = "configures"
	// EdgeMounts links a workload Resource to a volume source —
	// ConfigMap, Secret, or PersistentVolumeClaim — referenced from
	// `spec.volumes`. Direction: workload → volume source. Distinct
	// from EdgeConfigures (which is env-side wiring); EdgeMounts is
	// the filesystem-side wiring. Origin: ast_resolved.
	EdgeMounts EdgeKind = "mounts"
	// EdgeExposes links a Resource or Image to a port surface it
	// publishes. Source: K8s Service `spec.ports[]`, Deployment/Pod
	// `containerPorts[]`, Ingress rules, Dockerfile `EXPOSE`. Target:
	// a synthetic port node with ID `port::<proto>::<n>` (proto ∈
	// tcp|udp|http|https|grpc). Origin: ast_resolved.
	EdgeExposes EdgeKind = "exposes"
	// EdgeDependsOn captures runtime/build dependencies between
	// infrastructure entities — Ingress → Service backend, Service
	// → Pod (selector), Kustomization → base Kustomization,
	// Dockerfile stage → parent stage / external base Image,
	// Resource → Image. Direction: dependent → dependency. Origin:
	// ast_resolved.
	//
	// Also the model-lineage edge for the dbt / SQLMesh graph layer:
	// a dbt model → the models / seeds / snapshots it `ref()`s and the
	// sources it `source()`s, and a SQLMesh model → the models its
	// body reads via FROM / JOIN. Direction is the same (dependent →
	// dependency), so a downstream-impact walk over EdgeDependsOn
	// answers "what breaks if this model changes?" uniformly across
	// infra manifests and the transformation DAG. Edge Meta["link"]
	// disambiguates (ref|source|from).
	EdgeDependsOn EdgeKind = "depends_on"
	// EdgeUsesEnv links a Resource (workload) or Image (Dockerfile
	// stage) to a KindConfigKey representing an environment variable
	// it declares it needs at runtime. Direction: container surface
	// → config_key. The config_key ID convention `cfg::env::<NAME>`
	// matches what Go / Python / Node extractors emit for
	// `os.Getenv("NAME")` (and equivalents) so the cross-ref between
	// infra-side declaration and code-side consumption materialises
	// for free via shared node IDs. Origin: ast_resolved.
	EdgeUsesEnv EdgeKind = "uses_env"
	// EdgeSimilarTo links two function/method nodes whose bodies are
	// near-duplicates ("clones"). Materialised by the graph-wide
	// MinHash + LSH clone-detection pass: each function body is reduced
	// to a 64-slot MinHash signature at index time (stored on
	// Node.Meta["clone_sig"]), LSH banding produces candidate pairs,
	// and a Jaccard-similarity threshold filter keeps the true clones.
	// Emitted symmetrically — both fA→fB and fB→fA — so "what are the
	// clones of X" is a single out-edge walk from either endpoint.
	// Meta["similarity"] carries the estimated Jaccard score (0..1);
	// Confidence mirrors it. Origin: ast_inferred — the relationship is
	// a statistical estimate over normalised tokens, not a structural
	// fact. Pairs with dead-code analysis to surface "dead duplicates
	// of live code" — a near-duplicate of a live function that itself
	// has zero callers.
	EdgeSimilarTo EdgeKind = "similar_to"
	// EdgeSemanticallyRelated links two function/method nodes that are
	// plausibly semantically related without being near-duplicate
	// clones. Where EdgeSimilarTo captures only high direct token
	// overlap, this edge is materialised by a graph-diffusion smoothing
	// pass that runs after clone detection: it walks the EdgeSimilarTo
	// graph and, for a pair (A,C) joined through a shared similar
	// neighbour B, derives a diffused score from similarity(A,B) and
	// similarity(B,C). Transitive relatedness — "A is similar to B and
	// B is similar to C, so A and C are related" — surfaces pairs whose
	// direct Jaccard is below the clone threshold. Emitted symmetrically
	// — both fA→fB and fB→fA — so "what is X related to" is a single
	// out-edge walk from either endpoint. Meta["similarity"] carries the
	// diffused score (0..1); Confidence mirrors it. Origin: ast_inferred
	// — like EdgeSimilarTo the relationship is a statistical estimate
	// over normalised tokens, here additionally smoothed across the
	// similarity graph, not a structural fact. A pair that already has a
	// direct EdgeSimilarTo edge is never re-emitted as semantically
	// related — the two edge kinds partition cleanly.
	EdgeSemanticallyRelated EdgeKind = "semantically_related"
	// EdgeCoChange links two KindFile nodes that git history shows are
	// repeatedly committed together — "logical coupling". This is the
	// relationship the import graph cannot see: a handler and its
	// test, a struct and the serializer that mirrors it, a schema and
	// its migration are coupled even when neither imports the other.
	// Materialised by the cochange enrichment pass (`gortex enrich
	// cochange`, or lazily by the find_co_changing_symbols tool):
	// `git log --name-only` is mined for files that co-occur in a
	// commit, weighted by a cosine association score over per-file
	// commit-touch counts. Emitted symmetrically — both fA→fB and
	// fB→fA — so "what co-changes with X" is a single out-edge walk
	// from either endpoint. Meta["count"] carries the number of
	// commits touching both files; Meta["score"] (and Confidence)
	// carry the association strength (0..1). Origin: ast_inferred —
	// the relationship is a statistical estimate over git history,
	// not a structural fact.
	EdgeCoChange EdgeKind = "co_change"
	// Cross-repo edge kinds. Materialised by the resolver's
	// detectCrossRepoEdges pass: whenever a calls / implements / extends
	// edge has a From node and a To node in two different repos, a
	// parallel edge of the matching cross_repo_* kind is emitted
	// alongside the base edge (the base edge keeps its kind and also
	// gets Edge.CrossRepo set). The narrower kinds let
	// `analyze kind=cross_repo` walk only the repo-boundary-crossing
	// subset of the call / type-hierarchy graph without re-deriving the
	// boundary test from node RepoPrefix on every edge. From/To/FilePath/
	// Line/Origin/Confidence mirror the base edge; Origin is therefore
	// inherited (lsp_resolved for an LSP-confirmed call, ast_resolved
	// for a structural implements/extends, etc.). Idempotent —
	// graph.AddEdge dedupes by edgeKey — and incremental-safe — EvictFile
	// removes a node's edges in both directions, so a stale parallel
	// edge cannot survive a reindex of either endpoint's file.
	EdgeCrossRepoCalls      EdgeKind = "cross_repo_calls"
	EdgeCrossRepoImplements EdgeKind = "cross_repo_implements"
	EdgeCrossRepoExtends    EdgeKind = "cross_repo_extends"
	// EdgeCrossRepoMotivates parallels EdgeMotivates across a repo
	// boundary — knowledge in one repo (e.g. an org-wide ADR / docs repo)
	// explaining code in another tracked repo.
	EdgeCrossRepoMotivates EdgeKind = "cross_repo_motivates"
)

// CrossRepoKindFor maps a base edge kind to its parallel cross-repo
// variant. Only the call / type-hierarchy kinds named in M3 have a
// variant; every other kind returns ok == false. The mapping is the
// single source of truth for both the resolver's detectCrossRepoEdges
// pass and `analyze kind=cross_repo`.
func CrossRepoKindFor(base EdgeKind) (EdgeKind, bool) {
	switch base {
	case EdgeCalls:
		return EdgeCrossRepoCalls, true
	case EdgeImplements:
		return EdgeCrossRepoImplements, true
	case EdgeExtends:
		return EdgeCrossRepoExtends, true
	case EdgeMotivates:
		return EdgeCrossRepoMotivates, true
	}
	return "", false
}

// BaseKindForCrossRepo is the inverse of CrossRepoKindFor: it maps a
// cross-repo edge kind back to the base relation it parallels. Returns
// ok == false for any non-cross-repo kind.
func BaseKindForCrossRepo(cr EdgeKind) (EdgeKind, bool) {
	switch cr {
	case EdgeCrossRepoCalls:
		return EdgeCalls, true
	case EdgeCrossRepoImplements:
		return EdgeImplements, true
	case EdgeCrossRepoExtends:
		return EdgeExtends, true
	case EdgeCrossRepoMotivates:
		return EdgeMotivates, true
	}
	return "", false
}

// BaseKindsForCrossRepo returns the set of base edge kinds that have a
// parallel cross_repo_* variant. The slice is the single source of
// truth for callers (DetectCrossRepoEdges, the CrossRepoCandidates
// storage capability) that need the kind list without iterating
// CrossRepoKindFor over every edge.
func BaseKindsForCrossRepo() []EdgeKind {
	return []EdgeKind{EdgeCalls, EdgeImplements, EdgeExtends, EdgeMotivates}
}

type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Kind     EdgeKind `json:"kind"`
	FilePath string   `json:"file_path"`
	Line     int      `json:"line"`
	// Confidence is the numeric score (0..1). Kept on the in-memory
	// struct for internal filtering (min_tier, etc.) but excluded from
	// JSON — agents act on ConfidenceLabel, and the float adds ~15
	// chars to every edge in large graph responses.
	Confidence      float64 `json:"-"`
	ConfidenceLabel string  `json:"confidence_label,omitempty"`
	Origin          string  `json:"origin,omitempty"`
	// Tier is the coarse provenance label derived from Origin
	// (ast / lsp / heuristic). It is the agent-facing summary used by
	// retrieval UIs and competitor-parity columns (tokensave's
	// edges.resolved_by). Populated by enrichSubGraphEdges and the
	// dataflow encoders; empty by default on the in-memory edge.
	Tier      string `json:"tier,omitempty"`
	CrossRepo bool   `json:"cross_repo,omitempty"`
	// Context is the per-reference role a usage plays relative to the
	// target symbol — parameter_type, return_type, field, value, type,
	// attribute, generic_arg, or call. Empty on the stored edge; populated
	// on demand by RefContextOf when find_usages classifies a usage, so
	// agents can filter references by how they use the symbol
	// (`find_usages context:"parameter_type"`). Not part of the edge
	// identity / dedup key.
	Context string `json:"context,omitempty"`
	// ReturnUsage is how the call site consumes the callee's return
	// value — discarded, assigned, partially_ignored, returned,
	// goroutine, deferred, argument, or condition. Like Context it is
	// empty on the stored edge and populated on demand from
	// Meta[MetaReturnUsage] (stamped by the language extractors at
	// call-edge creation) when find_usages renders a usage, so agents
	// can ask "who actually uses the return?" before changing a return
	// signature. Not part of the edge identity / dedup key.
	ReturnUsage string `json:"return_usage,omitempty"`
	// Via is the human-readable provenance of a synthesized edge — the
	// framework-dispatch synthesizer that produced it (Swift↔ObjC bridge,
	// observer channel, React setState, …), derived from Meta["via"]. Empty
	// on directly-extracted edges; populated on demand by enrichSubGraphEdges
	// so traversal tools can render a "— via X" suffix. Not part of the edge
	// identity / dedup key.
	Via string `json:"via,omitempty"`
	// Alias is the renamed name carried by a per-binding import or a
	// re-export edge — the local name for `import { x as alias }` and the
	// exported name for `export { x as alias } from`. Empty when the binding
	// is not renamed (the local/exported name equals the upstream name) and
	// on every non-import edge. The edge's To still targets the upstream
	// (original) name, so the alias is the only place the renamed identifier
	// is recorded. Not part of the edge identity / dedup key.
	Alias string `json:"alias,omitempty"`
	// NameOnly marks a synthesized candidate row: a call site that names
	// the queried symbol but never bound to it, projected onto the symbol
	// so find_usages / get_callers can report what the graph does not
	// know. It is NOT a resolved binding and may belong to a different
	// same-named symbol entirely — the flag is what lets a consumer tell
	// it apart from a text_matched edge the resolver actually chose. Set
	// only on the in-memory projection under min_tier:"text_matched";
	// never persisted, never part of the edge identity / dedup key.
	NameOnly bool `json:"name_only,omitempty"`
	// Meta is intentionally excluded from JSON. It holds internal
	// instrumentation (semantic_source, provider hints, etc.) that agents
	// don't consume but that adds measurable bytes to every edge in
	// responses returning hundreds of call-graph edges. Internal callers
	// can still read/write the field; external MCP consumers don't see it.
	Meta map[string]any `json:"-"`
}

// ViaLabelFor maps a synthesizer Meta["via"] value to a human-readable
// provenance label for the "— via X" suffix on traversal output. Unmapped
// values pass through verbatim; an empty via yields an empty label.
func ViaLabelFor(via string) string {
	switch via {
	case "":
		return ""
	case "swift.objc.bridge":
		return "Swift↔ObjC bridge"
	case "observer.channel":
		return "observer channel"
	case "closure.collection":
		return "closure collection"
	case "react.setstate":
		return "React setState"
	case "flutter.setstate":
		return "Flutter setState"
	case "kmp.expect-actual":
		return "KMP expect/actual"
	case "rn.native.pair":
		return "RN native pairing"
	case "rn.bridge", "rn.native":
		return "RN bridge"
	case "event.channel":
		return "event channel"
	case "store-factory":
		return "store factory"
	default:
		return via
	}
}

// IdentityHash returns the edge's provenance-bearing identity: a stable
// 128-bit hash over (From, To, Kind, FilePath, Line, Origin). It is
// deliberately broader than the logical edgeKey the adjacency-list
// dedup index uses — that key omits Origin so a provenance upgrade
// replaces an edge in place instead of creating a parallel one. The
// identity hash, by contrast, treats Origin as load-bearing: two edges
// that differ only in Origin have different identities, so a silent
// provenance change is observable as an identity change.
//
// Changing an in-graph edge's Origin should therefore be modeled as a
// retirement of the old identity and creation of a new one — see
// Graph.SetEdgeProvenance, the only sanctioned mutation path.
func (e *Edge) IdentityHash() edgeHash {
	return hashEdgeIdentity(keyOf(e), e.Origin)
}

// Edge.Origin values — call-graph confidence tiers, highest → lowest. Use
// MeetsMinTier / OriginRank to compare.
//
//   - lsp_resolved: Compiler-grade. LSP, go/types, or SCIP confirms that this
//     edge's target is the precise symbol being referenced. Safe to rely on
//     for refactors.
//   - lsp_dispatch: Interface → implementation dispatch resolved by a
//     semantic provider. One step less direct than a literal target match.
//   - ast_resolved: Tree-sitter / AST extraction found a unique target in
//     the same compilation unit. No type system involved, but structurally
//     unambiguous.
//   - ast_inferred: Heuristic resolution using type info we extracted from
//     the AST. Not compiler-verified.
//   - text_matched: Name-only match. The weakest tier — could be a false
//     positive.
const (
	OriginLSPResolved = "lsp_resolved"
	OriginLSPDispatch = "lsp_dispatch"
	OriginASTResolved = "ast_resolved"
	OriginASTInferred = "ast_inferred"
	OriginTextMatched = "text_matched"
	// OriginSpeculative ranks strictly below text_matched: a best-guess
	// dynamic-dispatch edge (obj[name]() / getattr / decorator registry) that
	// is present-but-hidden-by-default. Always carries Meta[MetaSpeculative]=true
	// so queries exclude it unless the caller opts in.
	OriginSpeculative = "speculative"
)

// MetaSpeculative is the edge Meta key marking a speculative (best-guess)
// edge. It is the single source of truth for default-exclusion: any
// edge-returning surface drops these unless the caller opts in.
const MetaSpeculative = "speculative"

// DI-container binding contract (full shape documented in
// internal/parser/languages/di_container.go): container extractors emit
// EdgeProvides with these Meta keys; the resolver's provides-index and
// framework synthesizers consume them. Exported here so both sides
// compile against one spelling.
//
// Scoped to EdgeProvides. "binding" is not a reserved word elsewhere in
// the graph: the ORM extractors put an unrelated "binding" on model
// NODES (values "subclass" / "decorator" / "annotation" / "schema-macro",
// beside "orm" and "table_name") describing how a model was declared.
// The two never meet — different carrier, different values — so a
// key-name sweep must not fold them together.
const (
	MetaDIBinding       = "binding"        // one of the DIBinding* values below
	MetaDIProvidesFor   = "provides_for"   // interface short name the binding serves
	MetaDISource        = "di"             // declaring container ("spring", "autofac", …)
	MetaDIInterfaceFQCN = "interface_fqcn" // full interface name, for fidelity
	MetaDIImplFQCN      = "impl_fqcn"      // full implementation name
	// MetaDIToken names the injection token for the binding forms that
	// have no concrete class to point at (useValue / useFactory /
	// useExisting), where provides_for is absent.
	MetaDIToken = "di_token"
	// MetaDIOrigin records the provider method or decorator that
	// declared the binding. Unrelated to Edge.Origin, which records how
	// the edge itself was resolved.
	MetaDIOrigin = "origin"

	// Binding forms. useClass and bean name their abstract via
	// provides_for; the token forms use MetaDIToken instead.
	DIBindingUseClass      = "useClass"    // container maps an abstract to a concrete class
	DIBindingBean          = "bean"        // Spring @Bean factory method
	DIBindingUseValue      = "useValue"    // token bound to a literal value
	DIBindingUseFactory    = "useFactory"  // token bound to a factory function
	DIBindingUseExisting   = "useExisting" // token aliased to another provider
	DIBindingAsImplemented = "asImplementedInterfaces"
)

// MetaReparsePendingEnrichment is a KindFile-node Meta key (not an edge key)
// set by the indexer when a live watch re-parse resolved the file's references
// without re-running semantic enrichment — so the file's edges may sit below
// the tier the enrichment pass would mint. find_usages / get_callers read it
// to flag their default text_matched suppression as re-verification-pending,
// making a hidden-but-real usage diagnosable instead of silently dropped.
const MetaReparsePendingEnrichment = "reparse_pending_enrichment"

// OriginRank returns a numeric rank for origin comparison. Higher = more
// confident. Unknown or empty origin returns 0 so it sorts below all known
// tiers; filters treat it as "untagged" and fall back to legacy inference.
func OriginRank(origin string) int {
	switch origin {
	case OriginLSPResolved:
		return 6
	case OriginLSPDispatch:
		return 5
	case OriginASTResolved:
		return 4
	case OriginASTInferred:
		return 3
	case OriginTextMatched:
		return 2
	case OriginSpeculative:
		return 1
	}
	return 0
}

// IsSpeculative reports whether the edge is a best-guess speculative edge
// (excluded from default query results).
func (e *Edge) IsSpeculative() bool {
	if e == nil || e.Meta == nil {
		return false
	}
	v, _ := e.Meta[MetaSpeculative].(bool)
	return v
}

// MeetsMinTier returns true when origin is at least as confident as minTier.
// Empty minTier always passes (no filter). Empty origin fails any non-empty
// filter — callers wanting legacy fallback should first backfill via
// DefaultOriginFor.
func MeetsMinTier(origin, minTier string) bool {
	if minTier == "" {
		return true
	}
	return OriginRank(origin) >= OriginRank(minTier)
}

// ResolvedBy maps an Origin tier to a coarse provenance label used by
// agent retrieval UIs and competitor parity rows (cf. tokensave's
// `edges.resolved_by` column). The mapping collapses the five Origin
// tiers to three buckets:
//
//   - "lsp":       compiler-grade evidence (OriginLSPResolved / Dispatch)
//   - "ast":       structurally-unambiguous AST extraction
//   - "heuristic": name- or score-derived guess (ast_inferred / text_matched)
//
// Empty origin or unknown values return "heuristic" — a safe fallback
// for back-compat with graphs produced before Origin was stamped.
func ResolvedBy(origin string) string {
	switch origin {
	case OriginLSPResolved, OriginLSPDispatch:
		return "lsp"
	case OriginASTResolved:
		return "ast"
	case OriginASTInferred, OriginTextMatched:
		return "heuristic"
	}
	return "heuristic"
}

// DefaultOriginFor derives an origin tier for edges that don't have Origin
// set yet (edges from providers not updated to set Origin directly, or from
// indexes produced before this field existed). Uses edge kind, confidence
// score, and semantic_source meta as fallback signals. Never returns empty.
func DefaultOriginFor(kind EdgeKind, confidence float64, semanticSource string) string {
	if semanticSource != "" {
		if kind == EdgeImplements || kind == EdgeOverrides {
			return OriginLSPDispatch
		}
		return OriginLSPResolved
	}
	// Structural AST edges are unambiguous by construction.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeContains, EdgeExtends, EdgeMemberOf,
		EdgeImplements, EdgeProvides, EdgeConsumes, EdgeMatches, EdgeBridges,
		// Coverage structural edges: the extractor produces an
		// unambiguous source→target binding for each, so they share
		// the AST-resolved tier.
		EdgeParamOf, EdgeAliases, EdgeComposes, EdgeOverrides, EdgeLicensedAs,
		EdgeOwns, EdgeAuthored, EdgeGeneratedBy, EdgeDependsOnModule,
		EdgePackageWorkspaceMember,
		EdgeCaptures,
		// Framework-layer edges. Each is materialised by an extractor
		// that already proved the relationship (handler → route via
		// the contracts pipeline, model → table via the ORM detector,
		// parent → child via JSX walking) so they ride at ast_resolved.
		EdgeHandlesRoute, EdgeProducesTopic, EdgeConsumesTopic,
		EdgeModelsTable, EdgeRendersChild,
		// Dataflow edges. EdgeValueFlow is intra-procedural and
		// fully resolved at extraction. EdgeArgOf / EdgeReturnsTo
		// inherit ast_resolved once the post-resolution pass has
		// landed both ends; the dispatcher here just stamps the
		// default tier so freshly emitted edges classify cleanly.
		EdgeValueFlow, EdgeArgOf, EdgeReturnsTo,
		// Infrastructure-graph edges. Each is materialised by an
		// extractor (K8s/Kustomize/Dockerfile) that resolves the
		// relationship structurally from the manifest text.
		EdgeConfigures, EdgeMounts, EdgeExposes, EdgeDependsOn, EdgeUsesEnv,
		// Capability edges synthesised from already-structural edges
		// (reads_config / reads / writes) ride at the same tier.
		EdgeReadsEnv, EdgeAccessesField,
		// Cross-repo type-hierarchy edges parallel the structural
		// implements/extends edges, so they ride at the same tier.
		// EdgeCrossRepoCalls is intentionally excluded — it parallels
		// the resolution-derived `calls` edge and inherits that tier
		// via the confidence fallback below.
		EdgeCrossRepoImplements, EdgeCrossRepoExtends:
		return OriginASTResolved
	}
	// Resolution-derived edges fall back to confidence score.
	switch {
	case confidence >= 0.9:
		return OriginASTResolved
	case confidence >= 0.5:
		return OriginASTInferred
	}
	return OriginTextMatched
}

// EdgeTierScore maps an edge's Origin provenance tier to a 0..1 confidence
// weight, falling back to the edge kind when Origin is unstamped. It is the one
// shared provenance→confidence mapping consumed by the dataflow and callpath
// path-confidence rankers, so a path's confidence means the same thing across
// flow_between, taint_paths and trace_path.
func EdgeTierScore(origin string, kind EdgeKind) float64 {
	switch origin {
	case OriginLSPResolved:
		return 1
	case OriginLSPDispatch:
		return 0.95
	case OriginASTResolved:
		return 0.9
	case OriginASTInferred:
		return 0.7
	case OriginTextMatched:
		return 0.4
	}
	// Unstamped: fall back to the kind tier. value_flow is intra-procedural
	// and cheap to ground; arg_of / returns_to are cross-call and start lower
	// until the resolver lifts them.
	switch kind {
	case EdgeValueFlow:
		return 0.85
	case EdgeArgOf, EdgeReturnsTo:
		return 0.7
	}
	return 0.5
}

// Provenance-attenuation weights for graph centrality (HITS /
// PageRank) and the rerank provenance signal. Code intelligence
// enriched by LSP providers (gopls, clangd, tsserver) materialises a
// dense layer of framework-wiring and interface-dispatch edges;
// counting every such edge at full weight inflates the apparent
// centrality of utility and framework code over genuine domain
// authorities. Attenuating the abundant lsp tier — and the weak,
// possibly-spurious text-matched tier — relative to the
// structurally-unambiguous ast_resolved baseline rebalances authority
// toward real load-bearing code. ProvenanceWeightMin / Max bound the
// returned band so consumers can normalise onto [0,1].
const (
	ProvenanceWeightMin   = 0.5 // text_matched: weakest, possible false positive
	ProvenanceWeightMax   = 1.0 // ast_resolved: the trusted centrality baseline
	provWeightLSP         = 0.6 // lsp_resolved / lsp_dispatch: abundant, attenuate
	provWeightASTInferred = 0.8 // heuristic type inference: slight discount
)

// EffectiveOrigin returns the provenance tier to judge this edge by:
// Origin when the producer stamped one, otherwise the DefaultOriginFor
// backfill from kind + confidence + semantic_source.
//
// Reading e.Origin raw is almost always a bug. Most stored edges predate
// per-provider Origin stamping — in a 3.4M-edge graph, 141k resolved
// `calls` edges carry no Origin at all — and OriginRank maps the empty
// string to 0, below even OriginSpeculative. So a raw read silently sorts
// the single largest bucket of call edges *beneath* the weakest tagged
// tier. The backfill is also what agents are shown: enrichSubGraphEdges
// stamps the wire `origin` this way, so gating on EffectiveOrigin keeps a
// decision consistent with the provenance the same edge is displayed with.
// A nil edge reports the trusted baseline.
func (e *Edge) EffectiveOrigin() string {
	if e == nil {
		return OriginASTResolved
	}
	if e.Origin != "" {
		return e.Origin
	}
	sem, _ := e.Meta["semantic_source"].(string)
	return DefaultOriginFor(e.Kind, e.Confidence, sem)
}

// ProvenanceWeight returns the centrality weight for an edge based on
// its resolution provenance, in [ProvenanceWeightMin, ProvenanceWeightMax].
// Edges with an unset Origin are backfilled via DefaultOriginFor so
// pre-Origin indexes weight consistently. A nil edge weights at the
// trusted baseline.
func ProvenanceWeight(e *Edge) float64 {
	if e == nil {
		return ProvenanceWeightMax
	}
	switch e.EffectiveOrigin() {
	case OriginLSPResolved, OriginLSPDispatch:
		return provWeightLSP
	case OriginASTResolved:
		return ProvenanceWeightMax
	case OriginASTInferred:
		return provWeightASTInferred
	case OriginTextMatched:
		return ProvenanceWeightMin
	}
	return provWeightASTInferred
}

// Reference-context labels — the role a usage plays relative to the
// symbol it references. Mirrors graphify's REFERENCE_CONTEXTS so
// find_usages can filter "show me only the places this type is used as a
// parameter / return type / field …".
const (
	RefContextParameterType = "parameter_type"
	RefContextReturnType    = "return_type"
	RefContextField         = "field"
	RefContextValue         = "value"
	RefContextType          = "type"
	RefContextAttribute     = "attribute"
	RefContextGenericArg    = "generic_arg"
	RefContextCall          = "call"
	// RefContextImport marks an import / re-export statement that names the
	// symbol (`from x import name`, `import {name} from …`, `export {name}
	// from …`). LSP reference sets include these lines; find_usages can
	// isolate or exclude them via context:"import".
	RefContextImport = "import"
	// RefContextCallback marks a function referenced as a value to be invoked
	// later — registered as a callback / handler rather than called directly.
	RefContextCallback = "callback"
	// RefContextTemplate marks a reference that originates from a markup
	// template render site (`<Child />` in a Vue/Svelte/Astro component) rather
	// than from code. find_usages can then separate "rendered here" usages from
	// code-level references and show each positioned render location.
	RefContextTemplate = "template"
	// RefContextInherit marks a reference that names a supertype / conformed
	// protocol in a type declaration's inheritance clause (`class X: Base,
	// Proto`). It lets find_usages isolate "used as a base / conformance"
	// references from incidental type uses.
	RefContextInherit = "inherit"
	// RefContextCast marks a reference where the target type is used in a cast
	// or type test (`x as Foo`, `x as? Foo`, `x is Foo`) rather than a
	// declaration.
	RefContextCast = "cast"
	// RefContextStaticAccess marks a reference that reaches a type's static
	// member / nested type through the type name (`Foo.shared`,
	// `Foo.Constant`) rather than constructing or declaring it.
	RefContextStaticAccess = "static_access"
)

// RefContextOf classifies the reference context of an edge given the kind
// of its origin (usage-site) node. An extractor-stamped
// Meta["ref_context"] always wins (it lets a language extractor mark a
// context the structure alone can't reveal — e.g. generic_arg). Otherwise
// the context is derived from the edge kind plus the origin node kind,
// which Gortex's edge model already distinguishes:
//
//   - returns → return_type
//   - typed_as → parameter_type (from a param), field (from a field),
//     value (from a variable/constant/local), else type
//   - annotated → attribute
//   - reads / writes → value
//   - references / implements / extends / composes / aliases / overrides → type
//   - calls / instantiates → call
//
// Returns "" for edge kinds that carry no reference context.
func RefContextOf(e *Edge, fromKind NodeKind) string {
	if e == nil {
		return ""
	}
	if e.Meta != nil {
		if c, _ := e.Meta["ref_context"].(string); c != "" {
			return c
		}
		// A bound callback registration is a value-use of the function, not a
		// type reference — classify it so find_usages can filter callbacks.
		if v, _ := e.Meta["via"].(string); v == "callback_registration" {
			return RefContextCallback
		}
		// A markup render site (`<Child />`) is a template usage, not a code
		// type reference — classify it so find_usages can isolate render sites.
		if t, _ := e.Meta["template"].(bool); t {
			return RefContextTemplate
		}
	}
	switch e.Kind {
	case EdgeReturns:
		return RefContextReturnType
	case EdgeTypedAs:
		switch fromKind {
		case KindParam:
			return RefContextParameterType
		case KindField:
			return RefContextField
		case KindVariable, KindConstant, KindLocal:
			return RefContextValue
		}
		return RefContextType
	case EdgeAnnotated:
		return RefContextAttribute
	case EdgeReads, EdgeWrites:
		return RefContextValue
	case EdgeReferences, EdgeImplements, EdgeExtends, EdgeComposes, EdgeAliases, EdgeOverrides:
		return RefContextType
	case EdgeCalls, EdgeInstantiates:
		return RefContextCall
	case EdgeImports, EdgeReExports:
		return RefContextImport
	}
	return ""
}

// Return-usage labels — how a call site consumes the callee's return
// value. Stamped by the language extractors as Meta[MetaReturnUsage]
// on EdgeCalls edges at creation time, so the classification persists
// through every backend and survives reindexing.
const (
	// ReturnUsageDiscarded: the call is a bare expression statement (or
	// every result is bound to a blank sink, e.g. Go `_, _ = f()`).
	ReturnUsageDiscarded = "discarded"
	// ReturnUsageAssigned: the result is bound to variables or fields.
	ReturnUsageAssigned = "assigned"
	// ReturnUsagePartiallyIgnored: a multi-result call where some (but
	// not all) results are bound to blank sinks — Go `v, _ := f()`.
	ReturnUsagePartiallyIgnored = "partially_ignored"
	// ReturnUsageReturned: the call sits inside a return statement (or
	// the implicit-return tail position of an expression-bodied lambda
	// / Rust function / Ruby method).
	ReturnUsageReturned = "returned"
	// ReturnUsageGoroutine: the call is launched via a Go `go`
	// statement — the return value is unobservable.
	ReturnUsageGoroutine = "goroutine"
	// ReturnUsageDeferred: the call is a Go `defer` statement.
	ReturnUsageDeferred = "deferred"
	// ReturnUsageArgument: the result feeds another call — either as a
	// literal argument or as the receiver of a chained call.
	ReturnUsageArgument = "argument"
	// ReturnUsageCondition: the call sits inside an if / for / while /
	// switch condition.
	ReturnUsageCondition = "condition"
)

// MetaReturnUsage is the edge Meta key carrying the return-usage label
// of a call site. Single source of truth for the extractors that stamp
// it and the read paths (find_usages, verify_change) that surface it.
const MetaReturnUsage = "return_usage"

// ReturnUsageOf returns the extractor-stamped return-usage label of a
// call edge, or "" when the edge carries none (non-call edges, call
// shapes the classifier could not place).
func ReturnUsageOf(e *Edge) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	s, _ := e.Meta[MetaReturnUsage].(string)
	return s
}

// ConfidenceLabelRank orders the labels ConfidenceLabelFor produces,
// strongest first, for tie-breaking sorts; an unrecognized or empty
// label ranks lowest. Lives beside its producer so a new or renamed
// label cannot silently rank below the ones it outranks.
func ConfidenceLabelRank(label string) int {
	switch label {
	case "EXTRACTED":
		return 3
	case "INFERRED":
		return 2
	case "AMBIGUOUS":
		return 1
	}
	return 0
}

// ConfidenceLabelFor returns EXTRACTED, INFERRED, or AMBIGUOUS for an edge
// based on its kind and confidence value.
//
// Kept for back-compat; new code should prefer Origin tiers (OriginRank /
// MeetsMinTier) which distinguish LSP-grade from AST-grade evidence.
func ConfidenceLabelFor(kind EdgeKind, confidence float64) string {
	// Structural edges from AST are always extracted.
	switch kind {
	case EdgeDefines, EdgeImports, EdgeContains, EdgeExtends, EdgeMemberOf, EdgeImplements,
		EdgeProvides, EdgeConsumes, EdgeMatches, EdgeBridges,
		EdgeParamOf, EdgeAliases, EdgeComposes, EdgeOverrides, EdgeLicensedAs,
		EdgeOwns, EdgeAuthored, EdgeGeneratedBy, EdgeDependsOnModule,
		EdgePackageWorkspaceMember,
		EdgeCaptures,
		// Framework-layer edges. Each is materialised by an extractor
		// that already proved the relationship (handler → route via
		// the contracts pipeline, model → table via the ORM detector,
		// parent → child via JSX walking) so they ride at ast_resolved.
		EdgeHandlesRoute, EdgeProducesTopic, EdgeConsumesTopic,
		EdgeModelsTable, EdgeRendersChild,
		EdgeValueFlow, EdgeArgOf, EdgeReturnsTo,
		// Infrastructure-graph edges (K8s / Kustomize / Dockerfile).
		EdgeConfigures, EdgeMounts, EdgeExposes, EdgeDependsOn, EdgeUsesEnv,
		// Capability edges synthesised from structural edges.
		EdgeReadsEnv, EdgeAccessesField,
		// Cross-repo type-hierarchy edges parallel structural
		// implements/extends; EdgeCrossRepoCalls falls through to the
		// confidence-score classifier like the base `calls` edge.
		EdgeCrossRepoImplements, EdgeCrossRepoExtends:
		return "EXTRACTED"
	}
	// Resolution-derived edges: classify by confidence score.
	switch {
	case confidence >= 0.9:
		return "EXTRACTED"
	case confidence >= 0.5:
		return "INFERRED"
	case confidence > 0:
		return "AMBIGUOUS"
	default:
		// confidence == 0 means resolved without type info.
		return "INFERRED"
	}
}

// MetaGenericInstantiation is the edge Meta key marking a call whose
// callee spelled explicit type arguments. Some grammars cannot separate
// that from unrelated constructs — Go writes `Zero[int]()` exactly like
// indexing a func-valued map and `One[int](1)` exactly like converting
// to a generic type — so the extractor emits all of them and records
// which ones claimed to be instantiations. A resolver may then bind such
// an edge only to a declaration that really is generic, which is the one
// property a true instantiation guarantees and the impostors never have.
const MetaGenericInstantiation = "generic_instantiation"
