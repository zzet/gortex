package main

import (
 "fmt"
 "strings"
 "testing"
)

func moveProbeCorpus(seed int64) Corpus {
 c := editProbeCorpus()
 c.EditTool, c.EditKind = "refactor.move", "relocate"
 c.EditDestination = "destination.go"
 c.EditDestinationContent = "package main\n\nfunc DestinationAnchor() int { return 7 }\n"
 c.EditInitialContent = "package main\n\nfunc Example() int { return 1 }\n\n" + c.EditAnchor + "\n"
 for i := range 100 {
  name := fmt.Sprintf("GortexEditProbe%016xN%03dBefore", uint64(seed), i)
  c.EditInitialContent += strings.ReplaceAll(c.EditTemplate, "{{marker}}", name)
 }
 return c
}

func TestMovePlanPreservesFullScheduleAndDistinctSourceDestination(t *testing.T) {
 c := moveProbeCorpus(42)
 p, err := makeEditProbePlan(c, 42, 100)
 if err != nil { t.Fatal(err) }
 if len(p.Steps) != 100 { t.Fatal("lost offered moves") }
 for _, step := range p.Steps {
  name := step.Marker + "Before"
  if !step.Relocated || step.BeforeSymbol != c.File+"::"+name || step.TargetSymbol != c.EditDestination+"::"+name || step.Request.Tool != "refactor" || step.Request.Operation != "move" || step.Request.Arguments["destination"] != c.EditDestination || step.Request.Arguments["options"] != nil {
   t.Fatalf("move changed source authority: %+v", step)
  }
  if len(editProbeOracles(c, step)) != 8 { t.Fatal("missing positive or explicit absence oracle") }
 }
}

func TestMovePlanRefusesUnownedOrInapplicableFixture(t *testing.T) {
 for _, name := range []string{"nested_source", "nested_target", "same_file", "foreign_package", "preexisting_destination", "missing_source", "wrong_kind"} {
  t.Run(name, func(t *testing.T) {
   c := moveProbeCorpus(42)
   switch name {
   case "nested_source": c.File = "nested/sample.go"
   case "nested_target": c.EditDestination = "nested/destination.go"
   case "same_file": c.EditDestination = c.File
   case "foreign_package": c.EditDestinationContent = "package other\n"
   case "preexisting_destination": c.EditDestinationContent += "func GortexEditProbe000000000000002aN000Before() int { return 1 }\n"
   case "missing_source": c.EditInitialContent = strings.ReplaceAll(c.EditInitialContent, "GortexEditProbe000000000000002aN000Before", "Other")
   case "wrong_kind": c.EditKind = "append_leaf"
   }
   if _, err := makeEditProbePlan(c, 42, 100); err == nil { t.Fatal("unsupported move fixture admitted") }
  })
 }
}
