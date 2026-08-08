# You.com Web Search Integration

This feature adds web search capabilities to Gortex using the You.com Search API, complementing local code intelligence with current web information.

## Features

- **Web Search MCP Tool**: Adds `web_search` tool to Gortex's 100+ MCP tools
- **Flexible Authentication**: Supports both authenticated (YDC_API_KEY) and keyless modes
- **Smart Filtering**: Optional domain filtering to focus on authoritative sources
- **Error Handling**: Graceful fallback for API failures, rate limits, and network issues
- **Agent Integration**: Works seamlessly with all 19 supported AI coding agents

## Usage

### Basic Web Search

```json
{"name": "web_search", "arguments": {"query": "golang context cancellation patterns"}}
```

### Advanced Search with Domain Filtering

```json
{
  "name": "web_search", 
  "arguments": {
    "query": "React hooks best practices",
    "count": 5,
    "domains": ["reactjs.org", "stackoverflow.com", "developer.mozilla.org"]
  }
}
```

## Configuration

### Environment Variables

- **`YDC_API_KEY`** (optional): You.com API key for enhanced features and higher rate limits
- If no API key is provided, falls back to keyless You.com API with basic quota

### Example Agent Workflow

```
1. Agent analyzes local codebase using existing gortex tools
   → `search_symbols`, `find_usages`, `analyze` 

2. Agent searches web for current information
   → `web_search` with query: "React 18 concurrent features migration guide"

3. Agent combines local analysis with web context
   → Provides recommendations based on both local code and current best practices
```

## Implementation Details

- **Tool Registration**: Registered as standard MCP tool in gortex server
- **API Integration**: Uses You.com Search API with proper authentication headers
- **Response Format**: Structured search results with titles, URLs, and snippets
- **Rate Limiting**: Handles 429 responses with clear error messages
- **Security**: No credentials exposed in responses; follows existing gortex patterns

## Benefits for Code Intelligence

- **Documentation Discovery**: Find official docs and API references
- **Community Solutions**: Access Stack Overflow answers and GitHub issues  
- **Best Practices**: Current patterns and recommendations from the community
- **Library Updates**: Information about new versions and migration guides
- **Troubleshooting**: Error solutions and debugging techniques

## Testing

The integration includes comprehensive test coverage:

- Basic query handling and response formatting
- Parameter validation (count, domains, safesearch)
- Authentication header management
- Error scenarios (empty query, API failures)
- Response parsing and content generation

Run tests with:
```bash
go test -run TestWebSearch ./internal/mcp/
```

## Integration with Existing Tools

The `web_search` tool complements existing gortex capabilities:

| Gortex Tool | Web Search Enhancement |
|-------------|----------------------|
| `search_symbols` | Find external documentation for discovered symbols |
| `find_usages` | Research common usage patterns in the community |
| `analyze` | Compare local architecture with industry patterns |
| `contracts` | Find API specifications and integration examples |
| `review` | Check against current security best practices |

This creates a powerful combination of local code intelligence and global knowledge for AI coding agents.