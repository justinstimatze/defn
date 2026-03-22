// Package mcp implements the MCP server that exposes the defn database
// to Claude Code. This is the primary interface through which AI agents
// interact with code.
package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/justinstimatze/defn/internal/store"
)

// Server holds the MCP server state.
type Server struct {
	db *store.DB
}

// New creates a new MCP server backed by the given database.
func New(db *store.DB) *Server {
	return &Server{db: db}
}

// ToolDefinitions returns the MCP tool definitions for registration.
func (s *Server) ToolDefinitions() []ToolDef {
	return []ToolDef{
		{
			Name:        "get_definition",
			Description: "Get a code definition by name. Returns the full source, signature, doc comment, and metadata. Use this instead of reading files.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string", "description": "Definition name (e.g. 'Open', 'DB', 'HashBody')"},
					"module": map[string]any{"type": "string", "description": "Module path filter (optional, e.g. 'github.com/.../store')"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "search_definitions",
			Description: "Search for definitions by name pattern (SQL LIKE). Use % as wildcard. Returns matching names, kinds, signatures, and modules.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "SQL LIKE pattern (e.g. '%Handler%', 'Get%', '%')"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "get_callers",
			Description: "Get all definitions that call/reference a given definition. Use this to understand blast radius before making changes.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string", "description": "Definition name"},
					"module": map[string]any{"type": "string", "description": "Module path filter (optional)"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "get_callees",
			Description: "Get all definitions that a given definition calls/references. Use this to understand dependencies.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":   map[string]any{"type": "string", "description": "Definition name"},
					"module": map[string]any{"type": "string", "description": "Module path filter (optional)"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "update_definition",
			Description: "Update the body of an existing definition. The new body must be valid Go. This replaces the entire definition.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":     map[string]any{"type": "string", "description": "Definition name"},
					"module":   map[string]any{"type": "string", "description": "Module path filter (optional)"},
					"new_body": map[string]any{"type": "string", "description": "Complete new source text for this definition"},
				},
				"required": []string{"name", "new_body"},
			},
		},
		{
			Name:        "create_definition",
			Description: "Create a new definition (function, type, method, etc.) in a module.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"module": map[string]any{"type": "string", "description": "Module path"},
					"name":   map[string]any{"type": "string", "description": "Definition name"},
					"kind":   map[string]any{"type": "string", "enum": []string{"function", "method", "type", "interface", "const", "var"}},
					"body":   map[string]any{"type": "string", "description": "Full source text of the definition"},
				},
				"required": []string{"module", "name", "kind", "body"},
			},
		},
		{
			Name:        "list_modules",
			Description: "List all modules (packages) in the database.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "get_module_definitions",
			Description: "List all definitions in a module. Returns names, kinds, signatures, and export status.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"module": map[string]any{"type": "string", "description": "Module path"},
				},
				"required": []string{"module"},
			},
		},
		{
			Name:        "query",
			Description: "Run a raw SQL query against the defn database. Use for complex queries not covered by other tools. Tables: modules, definitions, references, imports, commits.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sql": map[string]any{"type": "string", "description": "SQL query to execute"},
				},
				"required": []string{"sql"},
			},
		},
		{
			Name:        "build",
			Description: "Emit files from the database and run `go build`. Returns any compilation errors.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{},
			},
		},
	}
}

// ToolDef represents an MCP tool definition.
type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
}

// HandleTool dispatches a tool call to the appropriate handler.
func (s *Server) HandleTool(name string, args map[string]any) (string, error) {
	switch name {
	case "get_definition":
		return s.handleGetDefinition(args)
	case "search_definitions":
		return s.handleSearchDefinitions(args)
	case "get_callers":
		return s.handleGetCallers(args)
	case "get_callees":
		return s.handleGetCallees(args)
	case "update_definition":
		return s.handleUpdateDefinition(args)
	case "create_definition":
		return s.handleCreateDefinition(args)
	case "list_modules":
		return s.handleListModules(args)
	case "get_module_definitions":
		return s.handleGetModuleDefinitions(args)
	case "query":
		return s.handleQuery(args)
	case "build":
		return s.handleBuild(args)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) handleGetDefinition(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	module, _ := args["module"].(string)

	d, err := s.db.GetDefinitionByName(name, module)
	if err != nil {
		return "", fmt.Errorf("definition %q not found: %w", name, err)
	}
	return toJSON(d)
}

func (s *Server) handleSearchDefinitions(args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	defs, err := s.db.FindDefinitions(pattern)
	if err != nil {
		return "", err
	}
	// Return summary, not full bodies.
	type summary struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Exported  bool   `json:"exported"`
		Receiver  string `json:"receiver,omitempty"`
		Signature string `json:"signature"`
		Module    int64  `json:"module_id"`
	}
	var results []summary
	for _, d := range defs {
		results = append(results, summary{
			ID: d.ID, Name: d.Name, Kind: d.Kind,
			Exported: d.Exported, Receiver: d.Receiver,
			Signature: d.Signature, Module: d.ModuleID,
		})
	}
	return toJSON(results)
}

func (s *Server) handleGetCallers(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	module, _ := args["module"].(string)

	d, err := s.db.GetDefinitionByName(name, module)
	if err != nil {
		return "", fmt.Errorf("definition %q not found: %w", name, err)
	}
	callers, err := s.db.GetCallers(d.ID)
	if err != nil {
		return "", err
	}
	return toJSON(callers)
}

func (s *Server) handleGetCallees(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	module, _ := args["module"].(string)

	d, err := s.db.GetDefinitionByName(name, module)
	if err != nil {
		return "", fmt.Errorf("definition %q not found: %w", name, err)
	}
	callees, err := s.db.GetCallees(d.ID)
	if err != nil {
		return "", err
	}
	return toJSON(callees)
}

func (s *Server) handleUpdateDefinition(args map[string]any) (string, error) {
	name, _ := args["name"].(string)
	module, _ := args["module"].(string)
	newBody, _ := args["new_body"].(string)

	d, err := s.db.GetDefinitionByName(name, module)
	if err != nil {
		return "", fmt.Errorf("definition %q not found: %w", name, err)
	}

	d.Body = newBody
	id, err := s.db.UpsertDefinition(d)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"updated": true, "id": %d, "hash": "%s"}`, id, store.HashBody(newBody)), nil
}

func (s *Server) handleCreateDefinition(args map[string]any) (string, error) {
	modulePath, _ := args["module"].(string)
	name, _ := args["name"].(string)
	kind, _ := args["kind"].(string)
	body, _ := args["body"].(string)

	// Look up module — it must exist.
	modules, err := s.db.ListModules()
	if err != nil {
		return "", err
	}
	var mod *store.Module
	for _, m := range modules {
		if m.Path == modulePath {
			mod = &m
			break
		}
	}
	if mod == nil {
		return "", fmt.Errorf("module %q not found", modulePath)
	}

	exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
	d := &store.Definition{
		ModuleID: mod.ID,
		Name:     name,
		Kind:     kind,
		Exported: exported,
		Body:     body,
	}
	id, err := s.db.UpsertDefinition(d)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"created": true, "id": %d}`, id), nil
}

func (s *Server) handleListModules(_ map[string]any) (string, error) {
	modules, err := s.db.ListModules()
	if err != nil {
		return "", err
	}
	return toJSON(modules)
}

func (s *Server) handleGetModuleDefinitions(args map[string]any) (string, error) {
	modulePath, _ := args["module"].(string)

	modules, err := s.db.ListModules()
	if err != nil {
		return "", err
	}
	for _, m := range modules {
		if m.Path == modulePath {
			defs, err := s.db.GetModuleDefinitions(m.ID)
			if err != nil {
				return "", err
			}
			return toJSON(defs)
		}
	}
	return "", fmt.Errorf("module %q not found", modulePath)
}

func (s *Server) handleQuery(args map[string]any) (string, error) {
	sql, _ := args["sql"].(string)
	results, err := s.db.Query(sql)
	if err != nil {
		return "", err
	}
	return toJSON(results)
}

func (s *Server) handleBuild(_ map[string]any) (string, error) {
	// TODO: emit files, run go build, return output
	return `{"status": "not yet implemented"}`, nil
}

func toJSON(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
