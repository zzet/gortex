package contracts

import (
	"testing"
)

func TestGRPCExtractor_ProtoProvider(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`
syntax = "proto3";

service UserService {
  rpc GetUser(GetUserRequest) returns (GetUserResponse) {}
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse) {}
}
`)
	contracts := ext.Extract("user.proto", src, nil, nil)
	if len(contracts) != 2 {
		t.Fatalf("expected 2 contracts, got %d", len(contracts))
	}
	assertContract(t, contracts[0], "grpc::UserService::GetUser", ContractGRPC, RoleProvider)
	assertContract(t, contracts[1], "grpc::UserService::ListUsers", ContractGRPC, RoleProvider)
}

func TestGRPCExtractor_GoConsumer(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`
package main

func main() {
	client := pb.NewUserServiceClient(conn)
}
`)
	contracts := ext.Extract("main.go", src, nil, nil)
	if len(contracts) < 1 {
		t.Fatalf("expected at least 1 contract, got %d", len(contracts))
	}
	assertContract(t, contracts[0], "grpc::UserService", ContractGRPC, RoleConsumer)
}

func TestGRPCExtractor_PythonConsumer(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`
stub = UserServiceStub(channel)
response = stub.GetUser(request)
`)
	contracts := ext.Extract("client.py", src, nil, nil)
	if len(contracts) < 1 {
		t.Fatalf("expected at least 1 contract, got %d", len(contracts))
	}
	assertContract(t, contracts[0], "grpc::UserService", ContractGRPC, RoleConsumer)
}

// TestGRPCExtractor_GoConsumer_MethodLevel covers the redesigned
// two-pass scan: per-method contracts with IDs matching the provider
// format "grpc::Service::Method", and SymbolID on the enclosing
// function so matcher pairing produces EdgeMatches bridges.
func TestGRPCExtractor_GoConsumer_MethodLevel(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package main

import (
	"context"
	"example.com/pb"
)

func makeRPCCall(ctx context.Context) {
	userClient := pb.NewUsersClient(conn)
	_, _ = userClient.GetUser(ctx, &pb.GetUserRequest{Id: "x"})
	_, _ = userClient.ListUsers(ctx, &pb.ListUsersRequest{})
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"makeRPCCall", 8, 12},
	})

	contracts := ext.Extract("main.go", src, nodes, nil)

	want := map[string]string{
		"grpc::Users::GetUser":   "main.go::makeRPCCall",
		"grpc::Users::ListUsers": "main.go::makeRPCCall",
	}
	got := map[string]string{}
	for _, c := range contracts {
		if c.Role == RoleConsumer && c.Type == ContractGRPC {
			got[c.ID] = c.SymbolID
		}
	}
	for id, wantSym := range want {
		gotSym, ok := got[id]
		if !ok {
			t.Errorf("missing consumer contract %s; all consumers: %v", id, got)
			continue
		}
		if gotSym != wantSym {
			t.Errorf("consumer %s: SymbolID want %q, got %q", id, wantSym, gotSym)
		}
	}

	// Fallback service-level contract must be suppressed when
	// method-level contracts already cover the service — otherwise
	// the registry fills with duplicates.
	for _, c := range contracts {
		if c.ID == "grpc::Users" {
			t.Errorf("unwanted service-level fallback emitted alongside method-level contracts: %+v", c)
		}
	}
}

// TestGRPCExtractor_GoConsumer_InlineChained covers the M4 inline
// chained form: pb.NewServiceClient(conn).Method(...) — the stub is
// constructed and the RPC invoked in one expression, with no
// intermediate variable for the two-pass scan to cross-reference.
func TestGRPCExtractor_GoConsumer_InlineChained(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package main

import (
	"context"
	"example.com/pb"
)

func makeRPCCall(ctx context.Context) {
	_, _ = pb.NewUsersClient(grpc.Dial(addr)).GetUser(ctx, &pb.GetUserRequest{Id: "x"})
}
`)
	nodes := makeNodes("main.go", []struct {
		name       string
		start, end int
	}{
		{"makeRPCCall", 8, 10},
	})

	contracts := ext.Extract("main.go", src, nodes, nil)

	var found *Contract
	for i := range contracts {
		c := contracts[i]
		if c.Role == RoleConsumer && c.ID == "grpc::Users::GetUser" {
			found = &contracts[i]
		}
	}
	if found == nil {
		t.Fatalf("missing inline-chained consumer contract grpc::Users::GetUser; got %+v", contracts)
	}
	if found.SymbolID != "main.go::makeRPCCall" {
		t.Errorf("SymbolID want main.go::makeRPCCall, got %q", found.SymbolID)
	}
	if found.Meta["request_type"] != "pb.GetUserRequest" {
		t.Errorf("request_type want pb.GetUserRequest, got %v", found.Meta["request_type"])
	}
	// The constructor's nested grpc.Dial(addr) call must not derail the
	// balance-scan into a service-level fallback duplicate.
	for _, c := range contracts {
		if c.ID == "grpc::Users" {
			t.Errorf("unwanted service-level fallback alongside inline-chained method contract: %+v", c)
		}
	}
}

// TestGRPCExtractor_GoConsumer_UnrelatedCallsAreNotGRPC guards the
// false-positive case: "(\w+).(\w+)(" matches every method call in a
// Go file, but we must only emit a gRPC consumer contract when the
// receiver was previously established as a gRPC client.
func TestGRPCExtractor_GoConsumer_UnrelatedCallsAreNotGRPC(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package main

func main() {
	logger.Info("hi")
	unrelated.Handle("msg")
}
`)
	contracts := ext.Extract("main.go", src, nil, nil)
	for _, c := range contracts {
		if c.Type == ContractGRPC {
			t.Errorf("unexpected gRPC contract %+v — no NewClient assignment exists", c)
		}
	}
}

// TestGRPCExtractor_RegisterServerDefinitionIsNotProvider guards the
// registration-site false positive: `Register<X>Server(` also matches a
// plain function definition (`func RegisterHTTPServer(mux ...)`) and
// non-gRPC helper calls. Minting a provider from those activates a
// latent `New<X>Client` consumer into a false exact-ID match. None of
// the inputs below name google.golang.org/grpc, so they must produce no
// provider contract.
func TestGRPCExtractor_RegisterServerDefinitionIsNotProvider(t *testing.T) {
	ext := &GRPCExtractor{}

	cases := map[string]string{
		"func definition": `package httpx

import "net/http"

// A plain registration helper — not a gRPC server registration.
func RegisterHTTPServer(mux *http.ServeMux) {
	mux.Handle("/", nil)
}
`,
		"bare helper call in grpc-free file": `package app

func boot() {
	RegisterMetricsServer(localRegistry)
}
`,
	}

	for name, src := range cases {
		contracts := ext.Extract("x.go", []byte(src), nil, nil)
		for _, c := range contracts {
			if c.Role == RoleProvider {
				t.Errorf("%s: unexpected provider contract %+v — no gRPC evidence present", name, c)
			}
		}
	}
}

// TestGRPCExtractor_RegisterServerNoFalseBridgePartner is the end-to-end
// guard for the activation chain: a file with a `New<X>Client` consumer
// plus a `func Register<X>Server` definition (the same service name)
// must NOT produce a same-ID provider, so the consumer stays an orphan
// and no false EdgeMatches / bridge can form.
func TestGRPCExtractor_RegisterServerNoFalseBridgePartner(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package app

func wire() {
	c := NewHTTPClient()
	_ = c
}

func RegisterHTTPServer(mux interface{}) {}
`)
	contracts := ext.Extract("app.go", []byte(src), nil, nil)
	var providers, consumers []Contract
	for _, c := range contracts {
		switch c.Role {
		case RoleProvider:
			providers = append(providers, c)
		case RoleConsumer:
			consumers = append(consumers, c)
		}
	}
	if len(providers) != 0 {
		t.Fatalf("expected no provider contracts from a func definition; got %+v", providers)
	}
	// The consumer side legitimately records grpc::HTTP, but with no
	// provider it can never pair into a bridge.
	for _, c := range consumers {
		if c.ID == "grpc::HTTP" {
			// Acceptable: an orphan consumer. Just assert no provider
			// shares its ID (already checked above).
			return
		}
	}
}

// TestGRPCExtractor_RegisterServerPackageQualifiedIsProvider pins the
// positive path: a generated-stub registration call
// (`pb.RegisterUsersServer(grpcServer, impl)`) is package-qualified, so
// it remains a service-level provider even without a grpc import.
func TestGRPCExtractor_RegisterServerPackageQualifiedIsProvider(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package main

import "context"

type UsersServer struct{}

func register(grpcServer interface{}) {
	pb.RegisterUsersServer(grpcServer, &UsersServer{})
}
`)
	contracts := ext.Extract("server.go", []byte(src), nil, nil)
	var found bool
	for _, c := range contracts {
		if c.Role == RoleProvider && c.ID == "grpc::Users" {
			found = true
			if reg, _ := c.Meta["registration"].(bool); !reg {
				t.Errorf("expected registration=true on the provider contract, got %+v", c.Meta)
			}
		}
	}
	if !found {
		t.Fatalf("expected provider contract grpc::Users from pb.RegisterUsersServer; got %+v", contracts)
	}
}

// TestGRPCExtractor_RegisterServerGRPCImportAllowsBareCall: a same-
// package registration with no selector still records when the file
// independently imports grpc.
func TestGRPCExtractor_RegisterServerGRPCImportAllowsBareCall(t *testing.T) {
	ext := &GRPCExtractor{}
	src := []byte(`package pb

import "google.golang.org/grpc"

func wire(s *grpc.Server, impl interface{}) {
	RegisterUsersServer(s, impl)
}
`)
	contracts := ext.Extract("wire.go", []byte(src), nil, nil)
	var found bool
	for _, c := range contracts {
		if c.Role == RoleProvider && c.ID == "grpc::Users" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected provider contract grpc::Users for a bare registration in a grpc-importing file; got %+v", contracts)
	}
}

func TestGRPCExtractor_TSClientsRequireGRPCEvidence(t *testing.T) {
	extractor := &GRPCExtractor{}
	for _, tc := range []struct {
		name, imports string
		want          bool
	}{
		{"LSP", `import { LanguageClient } from "vscode-languageclient";`, false},
		{"WebSocket", `import { PreviewClient } from "./preview";`, false},
		{"comment", `// import { credentials } from "@grpc/grpc-js";`, false},
		{"line comment", `// example; import "@grpc/grpc-js";`, false},
		{"block comment", "/*\nimport \"@grpc/grpc-js\";\n*/", false},
		{"template literal", "const sample = `\nimport \"@grpc/grpc-js\";\n`;", false},
		{"dynamic", `const { PreviewClient } = await import("./preview_pb.js");`, true},
		{"dynamic comment", `const { PreviewClient } = await import(/* chunk */ "./preview_pb.js");`, true},
		{"require comment", `const { PreviewClient } = require(/* note */ "./preview_pb.cjs");`, true},
		{"require later argument", `const other = require(resolvePath, "./preview_pb.cjs");`, false},
		{"import equals", `import grpc = require("@grpc/grpc-js");`, true},
		{"package prefix", `import "grpc-websocket";`, false},
		{"grpc-js", `import { credentials } from "@grpc/grpc-js";`, true},
		{"proto loader", `import * as protoLoader from "@grpc/proto-loader";`, true},
		{"Connect", `import { createClient } from "@connectrpc/connect";`, true},
		{"grpc-web", `import "grpc-web";`, true},
		{"generated", `import { PreviewClient } from "./preview_grpc_pb.js";`, true},
		{"require", `const { PreviewClient } = require("./preview_pb.cjs");`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.imports + "\nconst preview = new PreviewClient(open);\npreview.start();\nnew HubClient();\n")
			got := extractor.Extract("editor.ts", src, nil, nil)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("expected no gRPC contracts, got %+v", got)
				}
				return
			}
			findContract(t, got, "grpc::Preview::start", RoleConsumer)
			findContract(t, got, "grpc::Hub", RoleConsumer)
		})
	}
}

func assertContract(t *testing.T, c Contract, id string, ctype ContractType, role Role) {
	t.Helper()
	if c.ID != id {
		t.Errorf("expected ID %q, got %q", id, c.ID)
	}
	if c.Type != ctype {
		t.Errorf("expected Type %q, got %q", ctype, c.Type)
	}
	if c.Role != role {
		t.Errorf("expected Role %q, got %q", role, c.Role)
	}
}
