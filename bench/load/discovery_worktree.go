package main

import (
 "context"
 "errors"
 "fmt"
 "path/filepath"
 "strings"
 "os"
)

// Only this scenario alternates the two unrelated families. Existing worktree
// scenarios keep their primary-only topology and labels.
func (p *privateRun) addDiscoveryWorktree(ctx context.Context, family int) error {
 if family<0 || family>1 || len(p.Repos)<2 {return errors.New("discovery requires one of its two initial families")}
 primary:=p.Repos[family]
 ordinal:=0
 for _,repo:=range p.Repos {if strings.HasPrefix(repo.Corpus.Label,"discovery-linked-"){ordinal++}}
 label:=fmt.Sprintf("discovery-linked-%d-%d",family,ordinal)
 path:=filepath.Join(p.Root,"repos",label)
 if _,err:=p.git(ctx,primary.Path,"worktree","add","--detach",path,primary.Corpus.Commit);err!=nil{return err}
 file,err:=ownedFile(path,primary.Corpus.File);if err!=nil{return err}
 corpus:=primary.Corpus;corpus.Label=label
 p.Repos=append(p.Repos,privateRepo{Corpus:corpus,Path:path,File:file,Prefix:primary.Prefix})
 return nil
}

func(p *privateRun)writeDiscoveryBootRepos()([]privateRepo,error){
 if len(p.Repos)<2{return nil,errors.New("discovery requires two configured primaries")}
 configured:=*p;configured.Repos=p.Repos[:2]
 return configured.writeBootRepos(false)
}

func(p *privateRun)addExistingDiscoveryWorktree(ctx context.Context,ordinal int,nested bool)error{
 if len(p.Repos)<2{return errors.New("existing discovery topology requires two initial primaries")}
 primary:=p.Repos[0];label:=fmt.Sprintf("existing-linked-%d",ordinal)
 path:=filepath.Join(p.Root,"repos",label)
 if nested {path=filepath.Join(primary.Path,".load-existing",label);if err:=os.MkdirAll(filepath.Dir(path),0o700);err!=nil{return err}}
 if _,err:=p.git(ctx,primary.Path,"worktree","add","--detach",path,primary.Corpus.Commit);err!=nil{return err}
 file,err:=ownedFile(path,primary.Corpus.File);if err!=nil{return err}
 corpus:=primary.Corpus;corpus.Label=label;p.Repos=append(p.Repos,privateRepo{Corpus:corpus,Path:path,File:file,Prefix:primary.Prefix});return nil
}
