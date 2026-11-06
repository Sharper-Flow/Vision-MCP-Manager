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
    send({jsonrpc:'2.0',method:'notifications/resources/list_changed'});
    send({jsonrpc:'2.0',method:'notifications/prompts/list_changed'});
    send({jsonrpc:'2.0',id:msg.id,result:{content:[]}});
  }
});`

func TestSharedSubscriberCatalogNotifications(t *testing.T) {
	skipIfNoNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
	if _, err := ds.CallTool(ctx, &mcp.CallToolParams{Name: "notify"}); err != nil {
		t.Fatal(err)
	}
	for name, ch := range map[string]<-chan struct{}{"resources": resources, "prompts": prompts} {
		select {
		case <-ch:
		case <-ctx.Done():
			t.Errorf("%s callback not notified", name)
		}
	}
}
