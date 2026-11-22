package memoryservice

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gluonfield/jazmem/pkg/jazmem"
	"github.com/gluonfield/jazmem/pkg/jazmemhttp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wins/jaz/backend/internal/mcpsession"
)

const (
	PublicSearchToolName = "memory_search"
)

func (s *Service) AddMCPTools(server *mcp.Server) {
	tools := memoryTools{service: s}
	mcp.AddTool(server, &mcp.Tool{
		Name:        PublicSearchToolName,
		Title:       "Search Jaz memory",
		Description: "Search Jaz memory with the configured Memory agent. Returns a concise answer with source page references; search-agent tokens are recorded separately as Memory Search. Use memory_get_page for complete source text or edit context.",
	}, tools.Search)
	jazmemhttp.AddMCPGetPageTool(server, gatedJazmem{service: s})
}

func (s *Service) RemoveMCPTools(server *mcp.Server) {
	if server != nil {
		server.RemoveTools(PublicSearchToolName)
	}
	jazmemhttp.RemoveMCPGetPageTool(server)
}

func (s *Service) AddWorkerMCPTools(server *mcp.Server) {
	jazmemhttp.AddRawMCPTools(server, gatedJazmem{service: s})
}

func (s *Service) RemoveWorkerMCPTools(server *mcp.Server) {
	jazmemhttp.RemoveRawMCPTools(server)
}

func (s *Service) MCPToolsEnabled() bool {
	return s.Enabled()
}

type memoryTools struct {
	service *Service
}

type gatedJazmem struct {
	service *Service
}

type SearchInput struct {
	Query string `json:"query" jsonschema:"question or topic to answer from Jaz memory"`
	Limit int    `json:"limit,omitempty" jsonschema:"page limit, default 10, max 50"`
	Deep  bool   `json:"deep,omitempty" jsonschema:"wider retrieval with linked-page expansion"`
}

func (t memoryTools) Search(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return nil, nil, errors.New("query is required")
	}
	if err := gatedJazmem(t).ready(); err != nil {
		return nil, nil, err
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	answer, err := t.service.searcher.SearchMemory(ctx, SearchRequest{
		Query:    query,
		Limit:    min(limit, 50),
		Deep:     input.Deep,
		ParentID: mcpsession.SessionID(req),
	})
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: answer}}}, nil, nil
}

func (m gatedJazmem) Retrieve(ctx context.Context, query string, opts jazmem.SearchOptions) (jazmem.SearchResponse, error) {
	if err := m.ready(); err != nil {
		return jazmem.SearchResponse{}, err
	}
	return m.service.Memory.Retrieve(ctx, query, opts)
}

func (m gatedJazmem) GetPage(ctx context.Context, path string) (jazmem.Page, error) {
	if err := m.ready(); err != nil {
		return jazmem.Page{}, err
	}
	pagePath, err := normalizeMemoryPagePath(m.service.Memory.Root(), path)
	if err != nil {
		return jazmem.Page{}, err
	}
	return m.service.Memory.GetPage(ctx, pagePath)
}

func (m gatedJazmem) ready() error {
	if !m.service.Enabled() {
		return errors.New("memory is disabled in settings")
	}
	return nil
}

func normalizeMemoryPagePath(root, input string) (string, error) {
	pagePath := strings.TrimSpace(input)
	if pagePath == "" {
		return "", nil
	}
	if filepath.IsAbs(pagePath) {
		rel, ok := relativeMemoryPath(root, pagePath)
		if !ok {
			return "", fmt.Errorf("memory page path %q is outside memory root", pagePath)
		}
		if filepath.Ext(rel) != ".md" {
			return "", fmt.Errorf("memory page path %q is not a markdown page", pagePath)
		}
		pagePath = rel
	}
	pagePath = filepath.ToSlash(pagePath)
	pagePath = strings.TrimPrefix(pagePath, "./")
	pagePath = strings.TrimSuffix(pagePath, ".md")
	return strings.Trim(pagePath, "/"), nil
}

func relativeMemoryPath(root, pagePath string) (string, bool) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", false
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	cleanPath, err := filepath.Abs(pagePath)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == "." || rel == "" {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
