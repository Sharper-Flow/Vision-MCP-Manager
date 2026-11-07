package session

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const catalogNotificationsJS = `
const readline = require('readline');
const send = x => process.stdout.write(JSON.stringify(x) + '\n');
readline.createInterface({input: process.stdin}).on('line', line => {
  const msg = JSON.parse(line);
  if (msg.method === 'initialize') send({jsonrpc:'2.0', id:msg.id, result:{protocolVersion:msg.params.protocolVersion, capabilities:{tools:{},resources:{listChanged:true},prompts:{listChanged:true}}, serverInfo:{name:'fixture',version:'1'}}});
  if (msg.method === 'tools/call') {
    if (msg.params.name === 'emit_resources') send({jsonrpc:'2.0',method:'notifications/resources/list_changed'});
    if (msg.params.name === 'emit_prompts') send({jsonrpc:'2.0',method:'notifications/prompts/list_changed'});
    send({jsonrpc:'2.0',id:msg.id,result:{content:[]}});
  }
});`

func TestSharedSubscriberCatalogNotifications(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := testSharedServerConfig()
	cfg.Args = []string{"-e", catalogNotificationsJS}
	sm := NewSharedSessionManager("catalog", cfg, testLogger(t), 0, nil)
	defer sm.CloseAll()
	ds, err := sm.GetOrCreateSession(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	resources, prompts := make(chan struct{}, 1), make(chan struct{}, 1)
	sm.Subscribe("one", SharedSubscriber{
		OnResourceListChanged: func(context.Context) { resources <- struct{}{} },
		OnPromptListChanged:   func(context.Context) { prompts <- struct{}{} },
	})
	for _, tc := range []struct {
		tool        string
		want, other <-chan struct{}
	}{{"emit_resources", resources, prompts}, {"emit_prompts", prompts, resources}} {
		if _, err := ds.CallTool(ctx, &mcp.CallToolParams{Name: tc.tool}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-tc.want:
		case <-ctx.Done():
			t.Fatalf("%s callback not notified", tc.tool)
		}
		select {
		case <-tc.other:
			t.Fatalf("%s notified the wrong callback", tc.tool)
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
