package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/config"
	"github.com/Sharper-Flow/Vision-MCP-Manager/internal/session"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// capabilityFixtureServerJS is a downstream that serves a catalog-driven set
// of resources, resource templates, and prompts. FIXTURE_MODE=tools-only
// advertises and serves tools alone. The rotate_catalog tool reloads the
// catalog file and emits resources and prompts list_changed notifications.
const capabilityFixtureServerJS = `
const fs = require('fs');
const readline = require('readline');
const mode = process.env.FIXTURE_MODE || 'full';
let catalog = JSON.parse(fs.readFileSync(process.env.CATALOG_PATH, 'utf8'));
const send = (msg) => process.stdout.write(JSON.stringify(msg) + '\n');
const caps = () => mode === 'tools-only'
  ? { tools: { listChanged: true } }
  : { tools: { listChanged: true }, resources: { listChanged: true }, prompts: { listChanged: true } };
readline.createInterface({ input: process.stdin }).on('line', (line) => {
  const msg = JSON.parse(line);
  try {
    if (msg.method === 'initialize') {
      send({ jsonrpc: '2.0', id: msg.id, result: {
        protocolVersion: '2025-03-26', capabilities: caps(),
        serverInfo: { name: 'capability-fixture', version: '1.0.0' }
      }});
    } else if (msg.method === 'tools/list') {
      send({ jsonrpc: '2.0', id: msg.id, result: { tools: [
        { name: 'ping', inputSchema: { type: 'object' } },
        { name: 'rotate_catalog', inputSchema: { type: 'object' } }
      ]}});
    } else if (msg.method === 'tools/call' && msg.params.name === 'rotate_catalog') {
      catalog = JSON.parse(fs.readFileSync(process.env.CATALOG_PATH, 'utf8'));
      if (mode !== 'tools-only') {
        send({ jsonrpc: '2.0', method: 'notifications/resources/list_changed' });
        send({ jsonrpc: '2.0', method: 'notifications/prompts/list_changed' });
      }
      send({ jsonrpc: '2.0', id: msg.id, result: { content: [{ type: 'text', text: 'ok' }] } });
    } else if (msg.method === 'tools/call' && msg.params.name === 'ping') {
      send({ jsonrpc: '2.0', id: msg.id, result: { content: [{ type: 'text', text: 'pong' }] } });
    } else if (msg.method === 'resources/list') {
      if (mode === 'tools-only') { send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: 'Method not found' } }); return; }
      send({ jsonrpc: '2.0', id: msg.id, result: { resources: catalog.resources } });
    } else if (msg.method === 'resources/templates/list') {
      if (mode === 'tools-only') { send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: 'Method not found' } }); return; }
      send({ jsonrpc: '2.0', id: msg.id, result: { resourceTemplates: catalog.templates } });
    } else if (msg.method === 'resources/read') {
      const uri = msg.params.uri;
      const exact = catalog.resources.find((r) => r.uri === uri);
      if (exact) {
        send({ jsonrpc: '2.0', id: msg.id, result: { contents: [{ uri: exact.uri, mimeType: exact.mimeType, text: exact.text }] } });
      } else if (mode !== 'tools-only' && uri.startsWith('fixture://echo/')) {
        send({ jsonrpc: '2.0', id: msg.id, result: { contents: [{ uri, mimeType: 'text/plain', text: 'echo:' + uri.slice('fixture://echo/'.length) }] } });
      } else {
        send({ jsonrpc: '2.0', id: msg.id, error: { code: -32002, message: 'Unknown resource: ' + uri } });
      }
    } else if (msg.method === 'prompts/list') {
      if (mode === 'tools-only') { send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: 'Method not found' } }); return; }
      send({ jsonrpc: '2.0', id: msg.id, result: { prompts: catalog.prompts } });
    } else if (msg.method === 'prompts/get') {
      const p = catalog.prompts.find((x) => x.name === msg.params.name);
      if (!p) { send({ jsonrpc: '2.0', id: msg.id, error: { code: -32002, message: 'Unknown prompt' } }); return; }
      const who = (msg.params.arguments && msg.params.arguments.who) || 'nobody';
      send({ jsonrpc: '2.0', id: msg.id, result: { description: p.description, messages: [
        { role: 'user', content: { type: 'text', text: p.name + ' for ' + who } }
      ]}});
    } else if (msg.id !== undefined) {
      send({ jsonrpc: '2.0', id: msg.id, result: {} });
    }
  } catch (e) {
    process.stderr.write('Error: ' + e.message + '\n');
  }
});
`

// fixtureResource, fixtureTemplate, and fixturePrompt describe the catalog
// file. They mirror the wire format the fixture serves; Text rides along for
// resources/read and never appears in a listed Resource.
type fixtureResource struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

type fixtureTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	MimeType    string `json:"mimeType,omitempty"`
}

type fixturePrompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type fixtureCatalog struct {
	Resources  []fixtureResource `json:"resources"`
	Templates  []fixtureTemplate `json:"templates"`
	Prompts    []fixturePrompt   `json:"prompts"`
	StaticText string            `json:"-"`
}

func catalogV1() fixtureCatalog {
	return fixtureCatalog{
		Resources: []fixtureResource{{
			URI:      "fixture://static",
			Name:     "static",
			MimeType: "text/plain",
			Text:     "static-content-v1",
		}},
		Templates: []fixtureTemplate{{
			URITemplate: "fixture://echo/{word}",
			Name:        "echo",
			MimeType:    "text/plain",
		}},
		Prompts: []fixturePrompt{{
			Name:        "greet",
			Description: "greet someone",
		}},
	}
}

func catalogV2() fixtureCatalog {
	v2 := catalogV1()
	v2.Resources = append(v2.Resources, fixtureResource{
		URI:      "fixture://extra",
		Name:     "extra",
		MimeType: "text/plain",
		Text:     "extra-content-v2",
	})
	v2.Resources[0].Text = "static-content-v2"
	v2.Prompts = append(v2.Prompts, fixturePrompt{Name: "greet2", Description: "greet someone else"})
	return v2
}

func writeFixtureCatalog(t *testing.T, path string, catalog fixtureCatalog) {
	t.Helper()
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// fixtureCatalogSnapshot captures the catalog a client sees through a session.
type fixtureCatalogSnapshot struct {
	Resources  []*mcp.Resource
	Templates  []*mcp.ResourceTemplate
	Prompts    []*mcp.Prompt
	StaticText string
	GreetText  string
	EchoText   string
}

func promptText(t *testing.T, msg *mcp.PromptMessage) string {
	t.Helper()
	tc, ok := msg.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("prompt message content = %T, want *mcp.TextContent", msg.Content)
	}
	return tc.Text
}

func readCatalogSnapshot(t *testing.T, ctx context.Context, cs *mcp.ClientSession) fixtureCatalogSnapshot {
	t.Helper()
	snap := fixtureCatalogSnapshot{}
	res, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("resources/list: %v", err)
	}
	snap.Resources = res.Resources
	tmpl, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("resources/templates/list: %v", err)
	}
	snap.Templates = tmpl.ResourceTemplates
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	snap.Prompts = prompts.Prompts
	read, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fixture://static"})
	if err != nil {
		t.Fatalf("resources/read fixture://static: %v", err)
	}
	if len(read.Contents) != 1 {
		t.Fatalf("resources/read returned %d contents, want 1", len(read.Contents))
	}
	snap.StaticText = read.Contents[0].Text
	prompt, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"who": "world"}})
	if err != nil {
		t.Fatalf("prompts/get greet: %v", err)
	}
	if len(prompt.Messages) != 1 {
		t.Fatalf("prompts/get returned %d messages, want 1", len(prompt.Messages))
	}
	snap.GreetText = promptText(t, prompt.Messages[0])
	echo, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "fixture://echo/hello"})
	if err != nil {
		t.Fatalf("resources/read fixture://echo/hello: %v", err)
	}
	if len(echo.Contents) != 1 {
		t.Fatalf("template read returned %d contents, want 1", len(echo.Contents))
	}
	snap.EchoText = echo.Contents[0].Text
	return snap
}

func sortedResourcesJSON(t *testing.T, resources []*mcp.Resource) string {
	t.Helper()
	sort.Slice(resources, func(i, j int) bool { return resources[i].URI < resources[j].URI })
	data, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sortedTemplatesJSON(t *testing.T, templates []*mcp.ResourceTemplate) string {
	t.Helper()
	sort.Slice(templates, func(i, j int) bool { return templates[i].URITemplate < templates[j].URITemplate })
	data, err := json.Marshal(templates)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func sortedPromptsJSON(t *testing.T, prompts []*mcp.Prompt) string {
	t.Helper()
	sort.Slice(prompts, func(i, j int) bool { return prompts[i].Name < prompts[j].Name })
	data, err := json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func cloneResources(in []*mcp.Resource) []*mcp.Resource {
	out := make([]*mcp.Resource, len(in))
	copy(out, in)
	return out
}

func cloneTemplates(in []*mcp.ResourceTemplate) []*mcp.ResourceTemplate {
	out := make([]*mcp.ResourceTemplate, len(in))
	copy(out, in)
	return out
}

func clonePrompts(in []*mcp.Prompt) []*mcp.Prompt {
	out := make([]*mcp.Prompt, len(in))
	copy(out, in)
	return out
}

func assertCatalogParity(t *testing.T, direct, through fixtureCatalogSnapshot) {
	t.Helper()
	if got, want := sortedResourcesJSON(t, cloneResources(through.Resources)), sortedResourcesJSON(t, cloneResources(direct.Resources)); got != want {
		t.Errorf("resources through proxy = %s, want direct %s", got, want)
	}
	if got, want := sortedTemplatesJSON(t, cloneTemplates(through.Templates)), sortedTemplatesJSON(t, cloneTemplates(direct.Templates)); got != want {
		t.Errorf("resource templates through proxy = %s, want direct %s", got, want)
	}
	if got, want := sortedPromptsJSON(t, clonePrompts(through.Prompts)), sortedPromptsJSON(t, clonePrompts(direct.Prompts)); got != want {
		t.Errorf("prompts through proxy = %s, want direct %s", got, want)
	}
	if through.StaticText != direct.StaticText {
		t.Errorf("resources/read fixture://static through proxy = %q, want direct %q", through.StaticText, direct.StaticText)
	}
	if through.EchoText != direct.EchoText {
		t.Errorf("resources/read fixture://echo/hello through proxy = %q, want direct %q", through.EchoText, direct.EchoText)
	}
	if through.GreetText != direct.GreetText {
		t.Errorf("prompts/get greet through proxy = %q, want direct %q", through.GreetText, direct.GreetText)
	}
}

// connectDirectFixture connects a client straight to the fixture server,
// bypassing Vision, as the parity reference.
func connectDirectFixture(t *testing.T, ctx context.Context, catalogPath string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command("node", "-e", capabilityFixtureServerJS)
	cmd.Env = append(os.Environ(), "CATALOG_PATH="+catalogPath, "FIXTURE_MODE=full")
	client := mcp.NewClient(&mcp.Implementation{Name: "direct-fixture-client", Version: "1.0.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("direct fixture connect: %v", err)
	}
	return cs
}

func connectThroughProxy(t *testing.T, ctx context.Context, url string, onResourceChanged, onPromptChanged chan struct{}) *mcp.ClientSession {
	t.Helper()
	opts := &mcp.ClientOptions{}
	if onResourceChanged != nil {
		opts.ResourceListChangedHandler = func(context.Context, *mcp.ResourceListChangedRequest) {
			select {
			case onResourceChanged <- struct{}{}:
			default:
			}
		}
	}
	if onPromptChanged != nil {
		opts.PromptListChangedHandler = func(context.Context, *mcp.PromptListChangedRequest) {
			select {
			case onPromptChanged <- struct{}{}:
			default:
			}
		}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "proxy-fixture-client", Version: "1.0.0"}, opts)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("proxy connect: %v", err)
	}
	return cs
}

func fixtureServerConfig(t *testing.T, catalogPath, mode string) *config.ServerConfig {
	t.Helper()
	cfg := testServerConfig()
	cfg.Args = []string{"-e", capabilityFixtureServerJS}
	cfg.Env = map[string]string{"CATALOG_PATH": catalogPath, "FIXTURE_MODE": mode}
	return cfg
}

func TestProxyForwardsResourcesAndPrompts_PerSession(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	writeFixtureCatalog(t, catalogPath, catalogV1())
	logger := testLogger(t)
	mgr := session.NewManager("caps-per-session", fixtureServerConfig(t, catalogPath, "full"), logger)
	defer mgr.CloseAll()
	server := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "caps-per-session", SessionManager: mgr, Logger: logger,
	}))
	defer server.Close()

	direct := connectDirectFixture(t, ctx, catalogPath)
	defer direct.Close()
	through := connectThroughProxy(t, ctx, server.URL, nil, nil)
	defer through.Close()

	assertCatalogParity(t, readCatalogSnapshot(t, ctx, direct), readCatalogSnapshot(t, ctx, through))
}

func TestProxyForwardsResourcesAndPrompts_Shared(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	writeFixtureCatalog(t, catalogPath, catalogV1())
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("caps-shared", fixtureServerConfig(t, catalogPath, "full"), logger, 0, nil)
	defer sm.CloseAll()
	server := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "caps-shared", SharedManager: sm, Logger: logger,
	}))
	defer server.Close()

	direct := connectDirectFixture(t, ctx, catalogPath)
	defer direct.Close()
	directSnap := readCatalogSnapshot(t, ctx, direct)

	clients := make([]*mcp.ClientSession, 2)
	for i := range clients {
		clients[i] = connectThroughProxy(t, ctx, server.URL, nil, nil)
		defer clients[i].Close()
		assertCatalogParity(t, directSnap, readCatalogSnapshot(t, ctx, clients[i]))
	}
}

func TestProxyAdvertisesOnlyDownstreamCatalogCapabilities(t *testing.T) {
	skipIfNoNode(t)
	for _, mode := range []string{"per-session", "shared"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			catalogPath := filepath.Join(t.TempDir(), "catalog.json")
			writeFixtureCatalog(t, catalogPath, catalogV1())
			logger := testLogger(t)
			var server *httptest.Server
			if mode == "shared" {
				sm := session.NewSharedSessionManager("caps-none-shared", fixtureServerConfig(t, catalogPath, "tools-only"), logger, 0, nil)
				defer sm.CloseAll()
				server = httptest.NewServer(NewProxyHandler(ProxyConfig{
					ServerName: "caps-none-shared", SharedManager: sm, Logger: logger,
				}))
			} else {
				mgr := session.NewManager("caps-none-per-session", fixtureServerConfig(t, catalogPath, "tools-only"), logger)
				defer mgr.CloseAll()
				server = httptest.NewServer(NewProxyHandler(ProxyConfig{
					ServerName: "caps-none-per-session", SessionManager: mgr, Logger: logger,
				}))
			}
			defer server.Close()

			cs := connectThroughProxy(t, ctx, server.URL, nil, nil)
			defer cs.Close()
			caps := cs.InitializeResult().Capabilities
			if caps.Resources != nil {
				t.Errorf("advertised resources capability for a tools-only downstream: %+v", caps.Resources)
			}
			if caps.Prompts != nil {
				t.Errorf("advertised prompts capability for a tools-only downstream: %+v", caps.Prompts)
			}
			if caps.Tools == nil {
				t.Error("tools capability missing for a tools-only downstream")
			}
		})
	}
}

func awaitCatalogRefresh(t *testing.T, ctx context.Context, cs *mcp.ClientSession, resourceChanged, promptChanged <-chan struct{}, want fixtureCatalog) {
	t.Helper()
	wantResources := sortedResourcesJSON(t, cloneResources(expectedResources(want)))
	wantPrompts := sortedPromptsJSON(t, clonePrompts(expectedPrompts(want)))
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	gotResources, gotPrompts := false, false
	for !(gotResources && gotPrompts) {
		select {
		case <-resourceChanged:
			res, err := cs.ListResources(ctx, nil)
			if err != nil {
				t.Fatalf("resources/list after change: %v", err)
			}
			if sortedResourcesJSON(t, res.Resources) == wantResources {
				gotResources = true
			}
		case <-promptChanged:
			prompts, err := cs.ListPrompts(ctx, nil)
			if err != nil {
				t.Fatalf("prompts/list after change: %v", err)
			}
			if sortedPromptsJSON(t, prompts.Prompts) == wantPrompts {
				gotPrompts = true
			}
		case <-deadline.C:
			res, resErr := cs.ListResources(ctx, nil)
			prompts, promptErr := cs.ListPrompts(ctx, nil)
			t.Fatalf("catalog refresh not observed: resources=%v(err=%v) prompts=%v(err=%v); got resources=%s prompts=%s, want %s / %s",
				gotResources, resErr, gotPrompts, promptErr,
				sortedResourcesJSON(t, res.Resources), sortedPromptsJSON(t, prompts.Prompts), wantResources, wantPrompts)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func expectedResources(catalog fixtureCatalog) []*mcp.Resource {
	out := make([]*mcp.Resource, 0, len(catalog.Resources))
	for _, r := range catalog.Resources {
		out = append(out, &mcp.Resource{URI: r.URI, Name: r.Name, MIMEType: r.MimeType})
	}
	return out
}

func expectedPrompts(catalog fixtureCatalog) []*mcp.Prompt {
	out := make([]*mcp.Prompt, 0, len(catalog.Prompts))
	for _, p := range catalog.Prompts {
		out = append(out, &mcp.Prompt{Name: p.Name, Description: p.Description})
	}
	return out
}

func TestProxyRefreshesCatalogOnListChanged(t *testing.T) {
	skipIfNoNode(t)
	for _, mode := range []string{"per-session", "shared"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			catalogPath := filepath.Join(t.TempDir(), "catalog.json")
			writeFixtureCatalog(t, catalogPath, catalogV1())
			logger := testLogger(t)
			var server *httptest.Server
			if mode == "shared" {
				sm := session.NewSharedSessionManager("caps-refresh-shared", fixtureServerConfig(t, catalogPath, "full"), logger, 0, nil)
				defer sm.CloseAll()
				server = httptest.NewServer(NewProxyHandler(ProxyConfig{
					ServerName: "caps-refresh-shared", SharedManager: sm, Logger: logger,
				}))
			} else {
				mgr := session.NewManager("caps-refresh-per-session", fixtureServerConfig(t, catalogPath, "full"), logger)
				defer mgr.CloseAll()
				server = httptest.NewServer(NewProxyHandler(ProxyConfig{
					ServerName: "caps-refresh-per-session", SessionManager: mgr, Logger: logger,
				}))
			}
			defer server.Close()

			resourceChanged := make(chan struct{}, 8)
			promptChanged := make(chan struct{}, 8)
			cs := connectThroughProxy(t, ctx, server.URL, resourceChanged, promptChanged)
			defer cs.Close()
			readCatalogSnapshot(t, ctx, cs) // establish the v1 catalog first

			writeFixtureCatalog(t, catalogPath, catalogV2())
			if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "rotate_catalog"}); err != nil {
				t.Fatalf("rotate_catalog: %v", err)
			}
			awaitCatalogRefresh(t, ctx, cs, resourceChanged, promptChanged, catalogV2())
		})
	}
}

func TestSharedRespawnRefreshesCatalog(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	catalogPath := filepath.Join(t.TempDir(), "catalog.json")
	writeFixtureCatalog(t, catalogPath, catalogV1())
	logger := testLogger(t)
	sm := session.NewSharedSessionManager("caps-respawn", fixtureServerConfig(t, catalogPath, "full"), logger, 0, nil)
	defer sm.CloseAll()
	server := httptest.NewServer(NewProxyHandler(ProxyConfig{
		ServerName: "caps-respawn", SharedManager: sm, Logger: logger,
	}))
	defer server.Close()

	resourceChanged := make(chan struct{}, 8)
	promptChanged := make(chan struct{}, 8)
	cs := connectThroughProxy(t, ctx, server.URL, resourceChanged, promptChanged)
	defer cs.Close()
	readCatalogSnapshot(t, ctx, cs)

	writeFixtureCatalog(t, catalogPath, catalogV2())
	if err := sm.Downstream().Close(); err != nil {
		t.Fatal(err)
	}
	for sm.HasDownstream() {
		if ctx.Err() != nil {
			t.Fatal("manager did not observe downstream closure")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := sm.GetOrCreateSession(ctx, "fixture"); err != nil {
		t.Fatal(err)
	}
	awaitCatalogRefresh(t, ctx, cs, resourceChanged, promptChanged, catalogV2())
}
