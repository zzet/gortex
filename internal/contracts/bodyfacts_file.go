package contracts

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// GoBodyFactsForFile indexes one accepted file once, avoiding one root scan per
// function. It uses only the supplied AST/source and never a BindingResolver.
// Returned facts belong to the caller-owned tree and must not outlive it.
func GoBodyFactsForFile(ctx context.Context, tree *parser.ParseTree, nodes []*graph.Node) (map[string]BodyFacts, error) {
	if ctx == nil {
		return nil, fmt.Errorf("body facts: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tree == nil || tree.Tree() == nil || tree.Lang() != "go" {
		return nil, fmt.Errorf("body facts: incomplete accepted Go tree")
	}
	type indexedBody struct {
		name, receiver string
		body           *sitter.Node
	}
	bodies := make(map[int][]indexedBody)
	var visit func(*sitter.Node) error
	visit = func(node *sitter.Node) error {
		if node == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch node.Type() {
		case "function_declaration", "method_declaration", "func_literal":
			name := ""
			if named := node.ChildByFieldName("name"); named != nil {
				name = named.Content(tree.Source())
			}
			// Bodyless declarations are accepted Go syntax, not missing AST evidence.
			bodies[int(node.StartPoint().Row)+1] = append(bodies[int(node.StartPoint().Row)+1], indexedBody{name, goBodyFactsReceiver(node, tree.Source()), node.ChildByFieldName("body")})
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			if err := visit(node.NamedChild(i)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(tree.Tree().RootNode()); err != nil {
		return nil, err
	}
	out := make(map[string]BodyFacts)
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if node == nil || node.Kind != graph.KindFunction && node.Kind != graph.KindMethod {
			continue
		}
		var body *sitter.Node
		found := false
		for _, candidate := range bodies[node.StartLine] {
			receiver, _ := node.Meta["receiver"].(string)
			if candidate.name == node.Name && (node.Kind != graph.KindMethod || receiver == candidate.receiver) {
				if found {
					return nil, fmt.Errorf("body facts: ambiguous accepted function %s at line %d", node.ID, node.StartLine)
				}
				found = true
				body = candidate.body
			}
		}
		if !found && len(bodies[node.StartLine]) == 1 && bodies[node.StartLine][0].name == "" {
			found = true
			body = bodies[node.StartLine][0].body
		}
		if !found {
			return nil, fmt.Errorf("body facts: accepted function %s has no matching declaration at line %d", node.ID, node.StartLine)
		}
		facts := &goBodyFacts{tree: tree, src: tree.Source(), handler: node, body: body, bindings: make(map[string]Binding)}
		facts.walk(body)
		out[node.ID] = facts
	}
	return out, nil
}

func goBodyFactsReceiver(method *sitter.Node, src []byte) string {
	receiver := method.ChildByFieldName("receiver")
	if receiver == nil {
		return ""
	}
	var find func(*sitter.Node) string
	find = func(node *sitter.Node) string {
		if node == nil {
			return ""
		}
		if node.Type() == "type_identifier" {
			return node.Content(src)
		}
		for i := 0; i < int(node.NamedChildCount()); i++ {
			if found := find(node.NamedChild(i)); found != "" {
				return found
			}
		}
		return ""
	}
	return find(receiver)
}
