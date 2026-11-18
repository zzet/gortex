package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGraphQLExtractor_GoQueriesMustBeStringLiterals(t *testing.T) {
	extractor := &GraphQLExtractor{}
	src := []byte("package test\n" +
		"type query struct { _ int }\n" +
		"// query { fakeComment }\n" +
		"func use() {\n" +
		"  e.Query(ctx, query{})\n" +
		"  e.Query(ctx, `query {\r\n    rawField\r\n  }`)\n" +
		"  e.Query(ctx, \"query { quotedField }\")\n" +
		"}\n")
	got := extractor.Extract("query_test.go", src, nil, nil)
	if len(got) != 2 {
		t.Fatalf("expected only the two query documents, got %+v", got)
	}
	for i, want := range []struct {
		id   string
		line int
	}{
		{"graphql::Query::rawField", 6},
		{"graphql::Query::quotedField", 9},
	} {
		if got[i].ID != want.id || got[i].Line != want.line || got[i].Role != RoleConsumer {
			t.Errorf("contract %d = %+v; want %s at line %d", i, got[i], want.id, want.line)
		}
	}
}

func TestGraphQLExtractor_SchemaProvider(t *testing.T) {
	ext := &GraphQLExtractor{}
	src := []byte(`
type Query {
  users: [User]
  user(id: ID!): User
}

type Mutation {
  createUser(input: CreateUserInput!): User
}
`)
	contracts := ext.Extract("schema.graphql", src, nil, nil)
	if len(contracts) < 3 {
		t.Fatalf("expected at least 3 provider contracts, got %d", len(contracts))
	}

	ids := map[string]bool{}
	for _, c := range contracts {
		if c.Role != RoleProvider {
			continue
		}
		ids[c.ID] = true
	}
	for _, want := range []string{"graphql::Query::users", "graphql::Query::user", "graphql::Mutation::createUser"} {
		if !ids[want] {
			t.Errorf("missing expected contract %q, got ids: %v", want, ids)
		}
	}
}

func TestGraphQLExtractor_QueryConsumer(t *testing.T) {
	ext := &GraphQLExtractor{}
	src := []byte(`
const GET_USERS = gql` + "`" + `
  query {
    users {
      id
      name
    }
  }
` + "`" + `
`)
	contracts := ext.Extract("queries.ts", src, nil, nil)

	found := false
	for _, c := range contracts {
		if c.ID == "graphql::Query::users" && c.Role == RoleConsumer {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected consumer contract graphql::Query::users, got %v", contracts)
	}
}

func TestGraphQLExtractor_MutationConsumer(t *testing.T) {
	ext := &GraphQLExtractor{}
	src := []byte(`
mutation {
  createUser(input: {name: "Alice"}) {
    id
  }
}
`)
	contracts := ext.Extract("mutations.graphql", src, nil, nil)

	found := false
	for _, c := range contracts {
		if c.ID == "graphql::Mutation::createUser" && c.Role == RoleConsumer {
			found = true
			break
		}
	}
	if !found {
		// Also check providers since this is a .graphql file.
		for _, c := range contracts {
			if c.ID == "graphql::Mutation::createUser" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("expected contract graphql::Mutation::createUser, got %v", contracts)
	}
}

func TestGraphQLNestResolver(t *testing.T) {
	src := []byte(`import { Resolver, Query, Mutation, Args } from '@nestjs/graphql';

@Resolver(() => User)
export class UserResolver {
  @Query(() => [User])
  users() {
    return [];
  }

  @Mutation(() => User, { name: 'addUser' })
  createUser(@Args('input') input: CreateUserInput) {
    return null;
  }
}
`)
	nodes := []*graph.Node{
		nestMethodNode("r.ts", "users", 6),
		nestMethodNode("r.ts", "createUser", 11),
	}
	cs := (&GraphQLExtractor{}).Extract("r.ts", src, nodes, nil)

	q := nestFindByID(cs, "graphql::Query::users")
	if q == nil {
		t.Fatalf("expected graphql::Query::users from @Query, got %+v", cs)
	}
	if q.Role != RoleProvider || q.Meta["framework"] != "nestjs" {
		t.Errorf("query role/framework = %v/%v", q.Role, q.Meta["framework"])
	}
	if q.SymbolID != "r.ts::users" {
		t.Errorf("query handler = %q", q.SymbolID)
	}

	// Explicit name: option wins over the method name.
	m := nestFindByID(cs, "graphql::Mutation::addUser")
	if m == nil {
		t.Fatalf("expected graphql::Mutation::addUser (explicit name), got %+v", cs)
	}
	if m.SymbolID != "r.ts::createUser" {
		t.Errorf("mutation handler = %q", m.SymbolID)
	}
}
