package servicenow

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type req struct{ method, path, query, body string }

// session serves the tools over an in-memory transport against a fake instance.
func session(t *testing.T, status int) (*mcp.ClientSession, func() []req) {
	t.Helper()
	var mu sync.Mutex
	var seen []req
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, req{r.Method, r.URL.Path, r.URL.Query().Get("sysparm_query"), string(b)})
		mu.Unlock()
		if status != 200 {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"ACL denied"}}`)
			return
		}
		if r.Method == "GET" && !strings.HasSuffix(r.URL.Path, "00000000000000000000000000000000") && !strings.Contains(r.URL.Path, sysID) {
			io.WriteString(w, `{"result":[{"number":"INC0010001"}]}`)
			return
		}
		io.WriteString(w, `{"result":{"number":"INC0010001"}}`)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, Auth{APIKey: testKey}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverSide, clientSide := mcp.NewInMemoryTransports()
	go ServeMCP(ctx, c, serverSide)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs, func() []req { mu.Lock(); defer mu.Unlock(); return slices.Clone(seen) }
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string { return res.Content[0].(*mcp.TextContent).Text }

func TestToolList(t *testing.T) {
	cs, _ := session(t, 200)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}
	slices.Sort(got)
	want := []string{"add_work_note", "cmdb_lookup", "create_change", "create_incident", "get_change", "get_incident", "resolve_incident", "search_records", "update_incident"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools=%v", got)
	}
}

func TestGetIncidentByNumber(t *testing.T) {
	cs, seen := session(t, 200)
	res := call(t, cs, "get_incident", map[string]any{"number": "INC0010001"})
	if res.IsError || !strings.Contains(text(res), "INC0010001") {
		t.Fatalf("%+v", res)
	}
	r := seen()[0]
	if r.path != "/api/now/table/incident" || r.query != "number=INC0010001" {
		t.Fatalf("%+v", r)
	}
}

func TestNumbersCannotInjectQuery(t *testing.T) {
	cs, seen := session(t, 200)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_incident", map[string]any{"number": "INC0010001^ORactive=true"}},
		{"get_change", map[string]any{"number": "CHG1^active=true"}},
		{"cmdb_lookup", map[string]any{"name": "web^ORname=db"}},
	} {
		if res := call(t, cs, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s accepted %v", tc.tool, tc.args)
		}
	}
	if len(seen()) != 0 {
		t.Fatalf("requests sent: %v", seen())
	}
}

func TestChangeAndCmdbQueries(t *testing.T) {
	cs, seen := session(t, 200)
	call(t, cs, "get_change", map[string]any{"number": "CHG0030001"})
	call(t, cs, "cmdb_lookup", map[string]any{"name": "web01"})
	got := seen()
	if got[0].path != "/api/now/table/change_request" || got[0].query != "number=CHG0030001" ||
		got[1].path != "/api/now/table/cmdb_ci" || got[1].query != "nameLIKEweb01" {
		t.Fatalf("%+v", got)
	}
}

func TestAddWorkNote(t *testing.T) {
	cs, seen := session(t, 200)
	res := call(t, cs, "add_work_note", map[string]any{"sys_id": sysID, "text": "looking"})
	if res.IsError {
		t.Fatal(text(res))
	}
	r := seen()[0]
	if r.method != "PATCH" || r.path != "/api/now/table/incident/"+sysID || r.body != `{"work_notes":"looking"}` {
		t.Fatalf("%+v", r)
	}
}

func TestResolveIncident(t *testing.T) {
	cs, seen := session(t, 200)
	call(t, cs, "resolve_incident", map[string]any{"sys_id": sysID, "close_code": "Solution provided", "close_notes": "done"})
	if b := seen()[0].body; b != `{"close_code":"Solution provided","close_notes":"done","state":"6"}` {
		t.Fatalf("body=%s", b)
	}
}

func TestCreateIncidentOmitsEmptyFields(t *testing.T) {
	cs, seen := session(t, 200)
	call(t, cs, "create_incident", map[string]any{"short_description": "down", "urgency": "1"})
	if b := seen()[0].body; b != `{"short_description":"down","urgency":"1"}` {
		t.Fatalf("body=%s", b)
	}
}

func TestToolErrorIsResultNotProtocolError(t *testing.T) {
	cs, _ := session(t, 403)
	res := call(t, cs, "get_incident", map[string]any{"number": "INC0010001"})
	if !res.IsError || !strings.Contains(text(res), "403") || !strings.Contains(text(res), "ACL denied") {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(text(res), testKey) {
		t.Fatal("leaked key")
	}
}

func TestUpdateRefusesSysFields(t *testing.T) {
	cs, seen := session(t, 200)
	res := call(t, cs, "update_incident", map[string]any{"sys_id": sysID, "fields": map[string]any{"sys_created_by": "x"}})
	if !res.IsError || len(seen()) != 0 {
		t.Fatalf("%+v seen=%v", res, seen())
	}
}
