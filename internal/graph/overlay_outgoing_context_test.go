package graph_test

import (
 "context"
 "errors"
 "path/filepath"
 "reflect"
 "testing"

 "github.com/stretchr/testify/require"
 "github.com/zzet/gortex/internal/analysis"
 "github.com/zzet/gortex/internal/graph"
 "github.com/zzet/gortex/internal/graph/store_sqlite"
)

type outgoingContextReader interface {
 GetOutEdgesByNodeIDsContext(context.Context, []string, int) (map[string][]*graph.Edge, bool, error)
}

type outgoingContextSpy struct {
 graph.Reader
 contextual outgoingContextReader
 contextCalls int
 fullCalls int
 lastLimit int
 lastContext context.Context
 fail error
 cancel context.CancelFunc
}
func (s *outgoingContextSpy) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
 s.fullCalls++
 return s.Reader.GetOutEdgesByNodeIDs(ids)
}
func (s *outgoingContextSpy) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, limit int) (map[string][]*graph.Edge, bool, error) {
 s.contextCalls++; s.lastContext = ctx; s.lastLimit = limit
 rows, truncated, err := s.contextual.GetOutEdgesByNodeIDsContext(ctx, ids, limit)
 if s.cancel != nil { s.cancel() }
 if s.fail != nil { return rows, truncated, s.fail }
 return rows, truncated, err
}

// Keep the independent oracle on the original full Reader surface, even after
// the production view gains an optional contextual capability.
type fullOutgoingReader struct { graph.Reader }
func fullOutgoingChain(base graph.Reader, layers []graph.OverlayLayerReader) graph.Reader {
 for _, layer := range layers { base = graph.NewOverlaidViewWithLayer(base, layer) }
 return fullOutgoingReader{base}
}
func lightOutgoingRows(rows map[string][]*graph.Edge) map[string][]*graph.Edge {
 out := make(map[string][]*graph.Edge, len(rows))
 for id, edges := range rows {
  if edges != nil { out[id] = []*graph.Edge{} } else { out[id] = nil }
  for _, edge := range edges {
   if edge == nil { out[id] = append(out[id], nil); continue }
   out[id] = append(out[id], &graph.Edge{From: edge.From, To: edge.To, Kind: edge.Kind, FilePath: edge.FilePath, Line: edge.Line, Confidence: edge.Confidence, ConfidenceLabel: edge.ConfidenceLabel, Origin: edge.Origin, Tier: edge.Tier, CrossRepo: edge.CrossRepo})
  }
 }
 return out
}
func limitOutgoingOracle(rows map[string][]*graph.Edge, ids []string, limit int) (map[string][]*graph.Edge, bool) {
 if len(ids)==0 { return nil, false }
 out := make(map[string][]*graph.Edge); seen:=map[string]bool{}; total:=0
 for _, id:=range ids {
  if id=="" || seen[id] { continue }; seen[id]=true
  if limit<=0 { return nil,true }
  out[id]=rows[id][:0:0]
  for _, edge:=range rows[id] {
   if total>=limit { return out,true }
   out[id]=append(out[id],edge);total++
  }
 }
 return out,false
}
func contextualOutgoing(t *testing.T, view *graph.OverlaidView) outgoingContextReader {
 t.Helper()
 reader, ok := any(view).(outgoingContextReader)
 require.True(t, ok, "composed reader must preserve cancellable lightweight outgoing capability")
 return reader
}
func outgoingFixture(t *testing.T) (*outgoingContextSpy, []string, []graph.OverlayLayerReader) {
 t.Helper()
 store,err:=store_sqlite.Open(filepath.Join(t.TempDir(),"graph.sqlite"));require.NoError(t,err)
 t.Cleanup(func(){require.NoError(t,store.Close())})
 ids:=[]string{"a.go::A","b.go::B","c.go::C","foreign.go::F"}
 for _,id:=range ids { store.AddNode(&graph.Node{ID:id,Name:id,FilePath:graph.IDFile(id),Kind:graph.KindFunction}) }
 for _,edge:=range []*graph.Edge{
  {From:ids[0],To:ids[1],Kind:graph.EdgeCalls,Origin:graph.OriginASTResolved,FilePath:"a.go",Line:1,Meta:map[string]any{"meaning":"full payload","resolve_terminal":"pending"}},
  {From:ids[0],To:ids[2],Kind:graph.EdgeReferences,Origin:graph.OriginASTInferred,FilePath:"foreign.go",Line:2,Confidence:0.7},
  {From:ids[1],To:ids[0],Kind:graph.EdgeImplements,Origin:graph.OriginASTResolved,FilePath:"b.go",Line:3},
  {From:ids[3],To:ids[0],Kind:graph.EdgeCalls,Origin:graph.OriginASTResolved,FilePath:"foreign.go",Line:4},
 } { store.AddEdge(edge) }
 replace:=graph.NewOverlayLayer();replace.MarkFile("a.go",false);replace.AddNode("a.go",&graph.Node{ID:ids[0],FilePath:"a.go",Kind:graph.KindFunction});replace.AddEdge(&graph.Edge{From:ids[0],To:ids[2],Kind:graph.EdgeCalls,Origin:graph.OriginASTResolved,FilePath:"a.go",Line:8,Meta:map[string]any{"overlay":"owned"}})
 deleted:=graph.NewOverlayLayer();deleted.MarkFile("b.go",true);deleted.MarkRemoved("C",ids[2])
 restored:=graph.NewOverlayLayer();restored.MarkFile("b.go",false);restored.AddNode("b.go",&graph.Node{ID:ids[1],FilePath:"b.go",Kind:graph.KindFunction});restored.AddEdge(&graph.Edge{From:ids[1],To:ids[0],Kind:graph.EdgeImplements,Origin:graph.OriginASTResolved,FilePath:"b.go",Line:19})
 return &outgoingContextSpy{Reader:store,contextual:store},ids,[]graph.OverlayLayerReader{replace,deleted,restored}
}

func TestComposedOutgoingContextUsesLightCapabilityAndPreservesFullMetadata(t *testing.T) {
 base,ids,layers:=outgoingFixture(t)
 view:=graph.NewOverlaidViewWithLayers(base,layers)
 fullBefore:=fullOutgoingChain(base.Reader,layers).GetOutEdgesByNodeIDs(ids)
 reader:=contextualOutgoing(t,view)
 ctx,cancel:=context.WithCancel(t.Context());defer cancel()
 got,truncated,err:=reader.GetOutEdgesByNodeIDsContext(ctx,ids,100)
 require.NoError(t,err);require.False(t,truncated)
 require.Equal(t,lightOutgoingRows(fullBefore),lightOutgoingRows(got))
 require.Equal(t,1,base.contextCalls);require.Zero(t,base.fullCalls);require.Same(t,ctx,base.lastContext)
 require.Greater(t,base.lastLimit,100,"base cap must not be applied before overlay masking")
 require.Equal(t,fullBefore,view.GetOutEdgesByNodeIDs(ids),"generic reader must retain every full metadata field")
 require.Equal(t,"full payload",base.Reader.GetOutEdges(ids[0])[0].Meta["meaning"])
}

func TestComposedOutgoingContextMatchesIndependentFullOracleAcrossLayerOrders(t *testing.T) {
 base,ids,layers:=outgoingFixture(t)
 orders:=[][]int{{0,1,2},{0,2,1},{1,0,2},{1,2,0},{2,0,1},{2,1,0}}
 for _,order:=range orders {
  ordered:=[]graph.OverlayLayerReader{layers[order[0]],layers[order[1]],layers[order[2]]}
  for _,batch:=range []bool{false,true} {
   var view *graph.OverlaidView
   if batch { view=graph.NewOverlaidViewWithLayers(base,ordered) } else { view=fullOutgoingChain(base,ordered).(fullOutgoingReader).Reader.(*graph.OverlaidView) }
   reader:=contextualOutgoing(t,view);requested:=append(append([]string{},ids...),"",ids[0],"absent")
   original:=append([]string{},requested...);oracle:=fullOutgoingChain(base.Reader,ordered).GetOutEdgesByNodeIDs(requested)
   for _,limit:=range []int{0,1,2,100} {
    got,truncated,err:=reader.GetOutEdgesByNodeIDsContext(t.Context(),requested,limit)
    want,more:=limitOutgoingOracle(oracle,requested,limit)
    require.NoError(t,err);require.Equal(t,more,truncated);require.Equal(t,lightOutgoingRows(want),lightOutgoingRows(got));require.Equal(t,original,requested)
   }
   full:=fullOutgoingReader{view}
   want,wantStats:=analysis.BuildBoundedAdjacencySnapshot(full,ids,2,100,100)
   // A Reader adapter invokes only the optional contextual adjacency surface.
   projected:=outgoingAnalysisReader{Reader:full,contextual:reader,ctx:t.Context()}
   got,gotStats:=analysis.BuildBoundedAdjacencySnapshot(projected,ids,2,100,100)
   require.True(t,reflect.DeepEqual(want,got));require.Equal(t,wantStats,gotStats)
  }
 }
}
type outgoingAnalysisReader struct { graph.Reader; contextual outgoingContextReader; ctx context.Context }
func (r outgoingAnalysisReader) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge { rows,_,_:=r.contextual.GetOutEdgesByNodeIDsContext(r.ctx,ids,100000);return rows }

func TestComposedOutgoingContextCancellationErrorsAndFallback(t *testing.T) {
 base,ids,layers:=outgoingFixture(t);view:=graph.NewOverlaidViewWithLayers(base,layers);reader:=contextualOutgoing(t,view)
 ctx,cancel:=context.WithCancel(t.Context());cancel()
 got,_,err:=reader.GetOutEdgesByNodeIDsContext(ctx,ids,100);require.ErrorIs(t,err,context.Canceled);require.Empty(t,got);require.Zero(t,base.contextCalls)
 marker:=errors.New("partial base failure");base.fail=marker
 got,_,err=reader.GetOutEdgesByNodeIDsContext(t.Context(),ids,100);require.ErrorIs(t,err,marker);require.Empty(t,got)
 base.fail=nil;ctx,cancel=context.WithCancel(t.Context());base.cancel=cancel
 got,_,err=reader.GetOutEdgesByNodeIDsContext(ctx,ids,100);require.ErrorIs(t,err,context.Canceled);require.Empty(t,got)
 fallback:=graph.NewOverlaidViewWithLayers(fullOutgoingReader{base.Reader},layers)
 got,truncated,err:=contextualOutgoing(t,fallback).GetOutEdgesByNodeIDsContext(t.Context(),ids,100)
 require.NoError(t,err);require.False(t,truncated);require.Equal(t,fullOutgoingChain(base.Reader,layers).GetOutEdgesByNodeIDs(ids),got)
}

func TestComposedOutgoingContextCallerBoundariesRemainVisibleAfterMasking(t *testing.T) {
 base,ids,layers:=outgoingFixture(t);view:=graph.NewOverlaidViewWithLayers(base,layers)
 contextualOutgoing(t,view)
 got,cut:=graph.CallerBoundariesContext(t.Context(),view,ids,10)
 want,wantCut:=graph.CallerBoundariesContext(t.Context(),fullOutgoingChain(base.Reader,layers),ids,10)
 require.Equal(t,want,got);require.Equal(t,wantCut,cut)
}

// Emulate the existing SQLite light projection against a legacy full row.
// Empty Origin makes semantic_source relevant to the centrality weight.
type legacyLightOutgoing struct { graph.Reader }
func (r legacyLightOutgoing) GetOutEdgesByNodeIDsContext(ctx context.Context, ids []string, _ int) (map[string][]*graph.Edge, bool, error) { return lightOutgoingRows(r.Reader.GetOutEdgesByNodeIDs(ids)),false,ctx.Err() }
func TestComposedOutgoingContextLegacyOriginKeepsWeightedFullOracle(t *testing.T) {
 g:=graph.New();a,b:="a.go::A","b.go::B"
 g.AddNode(&graph.Node{ID:a,FilePath:"a.go",Kind:graph.KindFunction});g.AddNode(&graph.Node{ID:b,FilePath:"b.go",Kind:graph.KindFunction})
 edge:=&graph.Edge{From:a,To:b,Kind:graph.EdgeCalls,FilePath:"a.go",Meta:map[string]any{"semantic_source":"lsp","legacy":"must survive"}}
 g.AddEdge(edge)
 light:=*edge;light.Meta=nil
 require.NotEqual(t,graph.ProvenanceWeight(edge),graph.ProvenanceWeight(&light),"legacy fixture must expose weight loss")
 spy:=&outgoingContextSpy{Reader:g,contextual:legacyLightOutgoing{g}}
 layers:=[]graph.OverlayLayerReader{graph.NewOverlayLayer(),graph.NewOverlayLayer()};view:=graph.NewOverlaidViewWithLayers(spy,layers)
 rows,cut,err:=contextualOutgoing(t,view).GetOutEdgesByNodeIDsContext(t.Context(),[]string{a,b},100)
 require.NoError(t,err);require.False(t,cut);require.Equal(t,1,spy.contextCalls);require.Equal(t,1,spy.fullCalls)
 require.Equal(t,"must survive",rows[a][0].Meta["legacy"])
 require.Equal(t,graph.ProvenanceWeight(edge),graph.ProvenanceWeight(rows[a][0]))
 oracle:=fullOutgoingChain(g,layers)
 want,wantStats:=analysis.BuildBoundedAdjacencySnapshot(oracle,[]string{a},2,100,100)
 got,gotStats:=analysis.BuildBoundedAdjacencySnapshot(outgoingAnalysisReader{Reader:view,contextual:contextualOutgoing(t,view),ctx:t.Context()},[]string{a},2,100,100)
 require.True(t,reflect.DeepEqual(want,got));require.Equal(t,wantStats,gotStats)
}

func TestComposedOutgoingContextLimitIsAppliedOnlyAfterVisibility(t *testing.T) {
 base,ids,layers:=outgoingFixture(t)
 view:=graph.NewOverlaidViewWithLayers(base,layers[:1])
 got,cut,err:=contextualOutgoing(t,view).GetOutEdgesByNodeIDsContext(t.Context(),ids[:1],1)
 oracle:=fullOutgoingChain(base.Reader,layers[:1]).GetOutEdgesByNodeIDs(ids[:1])
 want,more:=limitOutgoingOracle(oracle,ids[:1],1)
 require.NoError(t,err);require.True(t,more);require.Equal(t,more,cut)
 require.Equal(t,lightOutgoingRows(want),lightOutgoingRows(got))
 require.Equal(t,2,got[ids[0]][0].Line,"masked first base row must not consume the visible cap")
}
