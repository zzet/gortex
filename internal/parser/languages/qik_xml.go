package languages

import (
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// QikXMLExtractor indexes ABA/Sabre QIK data descriptors exported as
// `.xml` under DATAITEM / TABLE trees (e.g. ITS.ABA.QIK). Documents are
// `LocalDescRef` roots with an `AppObjectDesc class="DATAITEM|TABLE"`.
//
// Surfaces:
//   - file node stamped qik_class + object name
//   - one symbol for the descriptor name (variable for DATAITEM, type for TABLE)
//   - TABLE ColumnDesc names as nested variables
//   - parentDescRef name → EdgeReferences (column → parent table hint)
//
// Shares the `.xml` extension with MyBatis / Spring / generic XML, so
// routing is content-sniffed (IsQikXML + detect_content.go). Non-QIK
// XML yields only the file node.
type QikXMLExtractor struct{}

func NewQikXMLExtractor() *QikXMLExtractor { return &QikXMLExtractor{} }

func (e *QikXMLExtractor) Language() string     { return "qikxml" }
func (e *QikXMLExtractor) Extensions() []string { return nil }

// IsQikXML reports whether src is an ABA QIK LocalDescRef descriptor.
// Cheap head scan — root LocalDescRef plus AppObjectDesc class DATAITEM
// or TABLE.
func IsQikXML(src []byte) bool {
	head := src
	const headCap = 8 * 1024
	if len(head) > headCap {
		head = head[:headCap]
	}
	lower := bytes.ToLower(head)
	if !bytes.Contains(lower, []byte("<localdescref")) {
		return false
	}
	return bytes.Contains(lower, []byte(`class="dataitem"`)) ||
		bytes.Contains(lower, []byte(`class="table"`)) ||
		bytes.Contains(lower, []byte(`class='dataitem'`)) ||
		bytes.Contains(lower, []byte(`class='table'`))
}

func (e *QikXMLExtractor) Extract(filePath string, src []byte) (*parser.ExtractionResult, error) {
	result := &parser.ExtractionResult{}
	fileNode := &graph.Node{
		ID:       filePath,
		Kind:     graph.KindFile,
		Name:     path.Base(filePath),
		FilePath: filePath,
		Language: "qikxml",
	}
	result.Nodes = append(result.Nodes, fileNode)

	if !IsQikXML(src) {
		return result, nil
	}

	lineStarts := lineStartOffsets(src)
	dec := xml.NewDecoder(bytes.NewReader(src))
	dec.Strict = false

	var (
		objectName string
		objectClass string // DATAITEM | TABLE
		maxLength   string
		inAppObject bool
		inColumns   bool
		inColumn    bool
		colName     string
		colMaxLen   string
		colLine     int
		depthApp    int
		depthCols   int
		depthCol    int
		seen        = map[string]bool{}
		// Stack of open element local names for path-ish text capture.
		stack []string
	)

	emitObject := func() {
		if objectName == "" || seen["obj:"+objectName] {
			return
		}
		seen["obj:"+objectName] = true
		kind := graph.KindVariable
		role := strings.ToUpper(objectClass)
		if role == "TABLE" {
			kind = graph.KindType
		}
		if role == "" {
			role = "DATAITEM"
		}
		id := filePath + "::" + objectName
		meta := map[string]any{
			"qik_class": role,
			"qik_role":  "descriptor",
		}
		if maxLength != "" {
			meta["qik_max_length"] = maxLength
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: kind, Name: objectName,
			FilePath: filePath, StartLine: 1, EndLine: 1,
			Language: "qikxml", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: 1,
		})
		fileNode.Meta = map[string]any{
			"qik_class":  role,
			"qik_object": objectName,
		}
	}

	emitColumn := func() {
		if colName == "" || seen["col:"+colName] {
			return
		}
		seen["col:"+colName] = true
		if colLine < 1 {
			colLine = 1
		}
		id := filePath + "::col:" + colName
		meta := map[string]any{"qik_role": "column"}
		if objectName != "" {
			meta["qik_table"] = objectName
		}
		if colMaxLen != "" {
			meta["qik_max_length"] = colMaxLen
		}
		result.Nodes = append(result.Nodes, &graph.Node{
			ID: id, Kind: graph.KindVariable, Name: colName,
			FilePath: filePath, StartLine: colLine, EndLine: colLine,
			Language: "qikxml", Meta: meta,
		})
		result.Edges = append(result.Edges, &graph.Edge{
			From: filePath, To: id, Kind: graph.EdgeDefines,
			FilePath: filePath, Line: colLine,
		})
		if objectName != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From: filePath + "::" + objectName, To: id, Kind: graph.EdgeDefines,
				FilePath: filePath, Line: colLine,
			})
		}
		colName, colMaxLen, colLine = "", "", 0
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := t.Name.Local
			stack = append(stack, local)
			line := lineForOffset(lineStarts, int(clampOffset(dec.InputOffset(), len(src))))

			switch strings.ToLower(local) {
			case "appobjectdesc":
				inAppObject = true
				depthApp = len(stack)
				if c := qikXMLAttr(t, "class"); c != "" {
					objectClass = c
				}
			case "columns":
				if inAppObject {
					inColumns = true
					depthCols = len(stack)
				}
			case "columndesc":
				if inColumns {
					inColumn = true
					depthCol = len(stack)
					colName, colMaxLen = "", ""
					colLine = line
				}
			case "parentdescref":
				// Optional reference to a parent descriptor name as child <name>.
			}
		case xml.EndElement:
			local := t.Name.Local
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			switch strings.ToLower(local) {
			case "appobjectdesc":
				if len(stack) < depthApp {
					inAppObject = false
				}
				emitObject()
			case "columns":
				if len(stack) < depthCols {
					inColumns = false
				}
			case "columndesc":
				if inColumn && len(stack) < depthCol {
					inColumn = false
					emitColumn()
				}
			}
		case xml.CharData:
			text := strings.TrimSpace(string(t))
			if text == "" || len(stack) == 0 {
				continue
			}
			cur := strings.ToLower(stack[len(stack)-1])
			parent := ""
			if len(stack) >= 2 {
				parent = strings.ToLower(stack[len(stack)-2])
			}
			// Top-level LocalDescRef/name before AppObjectDesc is the object name
			// in some exports; AppObjectDesc/name is authoritative when present.
			if cur == "name" {
				switch {
				case inColumn && parent == "columndesc":
					if colName == "" {
						colName = text
					}
				case inAppObject && !inColumn && parent == "appobjectdesc":
					objectName = text
				case !inAppObject && (parent == "localdescref" || parent == ""):
					if objectName == "" {
						objectName = text
					}
				case parent == "parentdescref":
					// Reference edge from file/object to parent name.
					if objectName != "" && text != "" {
						result.Edges = append(result.Edges, &graph.Edge{
							From: filePath + "::" + objectName,
							To:   "unresolved::qik::" + text,
							Kind: graph.EdgeReferences,
							FilePath: filePath,
							Meta: map[string]any{"via": "qik.parentDescRef"},
						})
					}
				}
			}
			if cur == "maxlength" {
				if inColumn {
					colMaxLen = text
				} else if inAppObject && maxLength == "" {
					maxLength = text
				}
			}
		}
	}
	// Flush if document ended mid-element without clean end tags.
	emitObject()
	if inColumn {
		emitColumn()
	}
	return result, nil
}

func qikXMLAttr(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if strings.EqualFold(a.Name.Local, local) {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

var _ parser.Extractor = (*QikXMLExtractor)(nil)
