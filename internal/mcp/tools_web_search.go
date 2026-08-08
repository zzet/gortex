package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"
)

// registerWebSearchTool registers the web_search MCP tool for You.com integration.
func (s *Server) registerWebSearchTool() {
	s.addTool(
		mcp.NewTool("web_search",
			mcp.WithDescription("Search the web using You.com for current information, documentation, API references, tutorials, Stack Overflow answers, and other external knowledge. Complements local code intelligence with web context."),
			mcp.WithString("query", mcp.Required(), mcp.Description("The search query. Be specific and use relevant keywords for programming topics, documentation, or technical issues.")),
			mcp.WithInteger("count", mcp.Description("Number of results to return (default: 10, max: 20)")),
			mcp.WithArray("domains", 
				mcp.Description("Optional list of domains to focus on (e.g., ['stackoverflow.com', 'docs.python.org']). Helps narrow results to authoritative sources."),
				mcp.WithStringItems(),
			),
			mcp.WithString("safesearch", mcp.Description("Safe search level: 'strict', 'moderate' (default), or 'off'")),
			mcp.WithString("country", mcp.Description("Country code for localized results (e.g., 'US', 'GB'). Defaults to global results.")),
		),
		s.handleWebSearch,
	)
}

// YouSearchResponse represents the response from You.com Search API
type YouSearchResponse struct {
	Results []YouSearchResult `json:"hits"`
}

// YouSearchResult represents a single search result from You.com
type YouSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// handleWebSearch implements the web_search MCP tool
func (s *Server) handleWebSearch(ctx context.Context, req mcp.CallToolRequest) (mcp.CallToolResult, error) {
	args := req.Params.Arguments
	
	// Extract and validate query
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return mcp.CallToolResult{
			IsError: true,
			Content: []interface{}{
				mcp.TextContent{
					Type: "text",
					Text: "Error: query parameter is required and must be a non-empty string",
				},
			},
		}, nil
	}

	// Parse count parameter
	count := 10 // default
	if countVal, exists := args["count"]; exists {
		if countFloat, ok := countVal.(float64); ok {
			count = int(countFloat)
		} else if countInt, ok := countVal.(int); ok {
			count = countInt
		}
	}
	if count < 1 || count > 20 {
		count = 10
	}

	// Parse domains parameter
	var domains []string
	if domainsVal, exists := args["domains"]; exists {
		if domainsArray, ok := domainsVal.([]interface{}); ok {
			for _, domain := range domainsArray {
				if domainStr, ok := domain.(string); ok {
					domains = append(domains, domainStr)
				}
			}
		}
	}

	// Parse safesearch parameter
	safesearch := "moderate" // default
	if safesearchVal, exists := args["safesearch"]; exists {
		if safesearchStr, ok := safesearchVal.(string); ok {
			switch strings.ToLower(safesearchStr) {
			case "strict", "moderate", "off":
				safesearch = strings.ToLower(safesearchStr)
			}
		}
	}

	// Parse country parameter
	var country string
	if countryVal, exists := args["country"]; exists {
		if countryStr, ok := countryVal.(string); ok {
			country = countryStr
		}
	}

	// Perform the search
	results, err := s.performYouSearch(ctx, query, count, domains, safesearch, country)
	if err != nil {
		s.logger.Error("Web search failed", zap.String("query", query), zap.Error(err))
		return mcp.CallToolResult{
			IsError: true,
			Content: []interface{}{
				mcp.TextContent{
					Type: "text",
					Text: fmt.Sprintf("Web search failed: %v", err),
				},
			},
		}, nil
	}

	// Format results
	var content []interface{}
	if len(results) == 0 {
		content = append(content, mcp.TextContent{
			Type: "text",
			Text: fmt.Sprintf("No web results found for query: %s", query),
		})
	} else {
		var resultText strings.Builder
		resultText.WriteString(fmt.Sprintf("Web search results for '%s' (%d results):\n\n", query, len(results)))
		
		for i, result := range results {
			resultText.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, result.Title))
			resultText.WriteString(fmt.Sprintf("   URL: %s\n", result.URL))
			resultText.WriteString(fmt.Sprintf("   %s\n\n", result.Snippet))
		}

		content = append(content, mcp.TextContent{
			Type: "text",
			Text: resultText.String(),
		})
	}

	return mcp.CallToolResult{
		Content: content,
	}, nil
}

// performYouSearch performs the actual web search using You.com API
func (s *Server) performYouSearch(ctx context.Context, query string, count int, domains []string, safesearch, country string) ([]YouSearchResult, error) {
	// Build the search URL
	baseURL := "https://api.you.com/search"
	params := url.Values{}
	params.Set("query", query)
	params.Set("count", strconv.Itoa(count))
	params.Set("safesearch", safesearch)
	
	if country != "" {
		params.Set("country", country)
	}
	
	if len(domains) > 0 {
		// Add domain filters to the query
		domainFilter := " site:" + strings.Join(domains, " OR site:")
		params.Set("query", query + domainFilter)
	}

	searchURL := baseURL + "?" + params.Encode()

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Add authentication if available
	apiKey := os.Getenv("YDC_API_KEY")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	
	req.Header.Set("User-Agent", "gortex/"+Version+" (https://github.com/zzet/gortex)")
	req.Header.Set("Accept", "application/json")

	// Perform the request
	client := &http.Client{
		Timeout: 30 * time.Second,
	}
	
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()

	// Handle different response codes
	switch resp.StatusCode {
	case http.StatusOK:
		// Success - parse response
		var searchResp YouSearchResponse
		if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
			return nil, fmt.Errorf("failed to parse search response: %w", err)
		}
		return searchResp.Results, nil
		
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("rate limited by You.com API - please wait and try again")
		
	case http.StatusUnauthorized:
		if apiKey == "" {
			return nil, fmt.Errorf("API key required - set YDC_API_KEY environment variable or use keyless quota")
		}
		return nil, fmt.Errorf("invalid API key - check YDC_API_KEY environment variable")
		
	case http.StatusPaymentRequired:
		return nil, fmt.Errorf("payment required - upgrade your You.com API plan or use keyless quota")
		
	default:
		return nil, fmt.Errorf("You.com API returned status %d", resp.StatusCode)
	}
}