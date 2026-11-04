package main

import (
 "errors"
 "flag"
 "fmt"
 "io"
 "reflect"
)

// This demonstration gate has a measured candidate-OFF denominator. It does
// not add a read-only absolute release target or replace edit4's 400ms gate.
func readOnlySeedCommand(args []string, stdout, stderr io.Writer) int {
 f:=flag.NewFlagSet("seed-search",flag.ContinueOnError); f.SetOutput(stderr)
 offPath:=f.String("off-inputs","","existing strict MAIN-OFF and candidate-OFF inputs")
 onPath:=f.String("on-inputs","","strict candidate-ON read16 inputs")
 host:=f.String("host-class","","exact reviewed host class")
 output:=f.String("out","","optional NEW JSON evidence output")
 if err:=f.Parse(args);err!=nil { if errors.Is(err,flag.ErrHelp){return 0};return 2 }
 if f.NArg()!=0 || *offPath=="" || *onPath=="" || !knownHostClass(*host) {fmt.Fprintln(stderr,"--off-inputs, --on-inputs and --host-class are required");return 2}
 off,err:=loadInputs(*offPath);if err!=nil {fmt.Fprintln(stderr,err);return 2}
 on,err:=loadInputs(*onPath);if err!=nil {fmt.Fprintln(stderr,err);return 2}
 rows,err:=readOnlySeedRows(off,on,*host);if err!=nil {fmt.Fprintln(stderr,err);return 2}
 pass:=printRows(stdout,rows)
 doubled:=rows[1].After!=nil && rows[1].Limit!=nil && *rows[1].After>=*rows[1].Limit
 valid:=rows[0].Pass && doubled
 fmt.Fprintf(stdout,"measured_doubling=%t demonstration_valid=%t (no full-smoke or nightly-history credit)\n",doubled,valid)
 if *output!="" {
  evidence:=struct{Rows []result `json:"rows"`; MeasuredDoubling bool `json:"measured_doubling"`; DemonstrationValid bool `json:"demonstration_valid"`}{rows,doubled,valid}
  if err:=writeNewJSON(*output,evidence);err!=nil {fmt.Fprintln(stderr,err);return 2}
 }
 if !pass {return 1};return 0
}

func readOnlySeedRows(off,on dataset,host string)([]result,error) {
 a,b:=off.config,on.config
 if a.NightlySeed!=nil || b.NightlySeed==nil || b.NightlySeed.Kind!="search-delay-read16-v1" || a.CandidateRevision!=b.CandidateRevision || a.MainRevision!=b.MainRevision || a.TagRevision!=b.TagRevision || !reflect.DeepEqual(a.Hosts,b.Hosts) || len(a.Hosts)!=1 || a.Hosts[host]=="" || a.repeats("load-16")!=b.repeats("load-16") {return nil,errors.New("read16 OFF/ON source/host/repeat/seed identities differ")}
 n:=a.repeats("load-16")
 if len(off.runs)!=2*n || len(on.runs)!=n {return nil,errors.New("read16 demonstration needs only MAIN-OFF, candidate-OFF and candidate-ON for every planned repeat")}
 identity:=map[string]bool{}
 for _,group:=range []dataset{off,on} {for _,r:=range group.runs {
  if r.input.Scenario!="load-16" || r.input.HostClass!=host || identity[r.data.RunID] {return nil,errors.New("foreign scenario/host or reused run identity")}; identity[r.data.RunID]=true
 }}
 spec:=budget{Scenario:"load-16",Metric:"search_symbols_p95",HostClass:host,Mode:"block",Unit:"s",Compare:"le"}
 main,mr,err:=off.samples("main",spec);if err!=nil{return nil,err}
 clean,cr,err:=off.samples("candidate",spec);if err!=nil{return nil,err}
 seeded,sr,err:=on.samples("candidate",spec);if err!=nil{return nil,err}
 for i:=range cr {if !comparable(cr[i],mr[i]) || !comparable(cr[i],sr[i]) {return nil,errors.New("matched read16 source/corpus/workload/deadline identities differ")}}
 before,err:=median(main);if err!=nil || before<=0 {return nil,errors.New("measured MAIN search denominator must be positive")}
 baseline,err:=median(clean);if err!=nil || baseline<=0 {return nil,errors.New("measured candidate-OFF search denominator must be positive")}
 cleanLimit:=before*1.2;spec.Target=&cleanLimit
 cleanRow:=assess(policy{},spec,clean,main,nil,"release");cleanRow.Baseline="measured MAIN-OFF (literal smoke 20% comparison)"
 doubleLimit:=baseline*2;spec.Target=&doubleLimit;spec.Compare="lt"
 seededRow:=assess(policy{},spec,seeded,clean,nil,"release");seededRow.Baseline="measured candidate-OFF (literal doubling demonstration only)"
 if !seededRow.Pass {seededRow.Reason="measured search_symbols p95 doubled or worse; read-only seeded demonstration breach"}
 return []result{cleanRow,seededRow},nil
}
