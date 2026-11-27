// Package m2mgraphql exposes the single, versioned SemReg PV query.
package m2mgraphql

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/printer"
)

const (
	route                     = "/graphql/m2m/v1"
	semanticPVContractID      = "PUBLIC_GRAPHQL_SEMANTIC_PV_V1"
	semanticStorageContractID = "PUBLIC_GRAPHQL_SEMANTIC_STORAGE_V1"
	semanticEVSEContractID    = "PUBLIC_GRAPHQL_SEMANTIC_EVSE_V1"
	maxRequestBytes           = 16 << 10
	maxResponseBytes          = 4 << 20
	maxJSONDepth              = 64
	maxQueryDepth             = 8
	maxSelectedFields         = 256
)

// The request-budget table is bounded because remote hosts are not
// authenticated: a client rotating source addresses (for example across an
// IPv6 prefix) would otherwise create one permanent entry per address.
//
// rateLimitTableCapacity bounds the budget entries. Inserting a new key at
// capacity first evicts idle entries, then the least recently used entry; a
// new key always receives an entry and is never refused for table pressure.
// An entry whose request is in flight is never evicted, by the idle sweep or
// by LRU, so its one-in-flight guard always holds. When every entry is in
// flight the table grows past capacity instead; the table is therefore bounded
// by rateLimitTableCapacity plus the number of in-flight requests, which the
// HTTP server already bounds, and the next insertion of a new key trims it
// back to capacity once those requests are released. rateLimitIdleMillis is
// the idle window after which an untouched entry is evicted. Any entry idle
// for longer than three refill intervals is already back at its full budget, so
// idle eviction cannot change admission; only LRU eviction under pressure can,
// and it can only grant a returning idle key a fresh budget, never deny one.
// Explicit in-process partitions ("key:") share the same table and policy.
const (
	rateLimitTableCapacity = 4096
	rateLimitIdleMillis    = 60_000
	rateLimitBurst         = 3
	rateLimitRefillMillis  = 1000
)

const semanticResourcesFixedQuery = `query SemanticResources($cut_id: String, $page_size: Int!, $page_token: String, $kind: String, $plane: String) {
  semantic_resources(cut_id: $cut_id, page_size: $page_size, page_token: $page_token, kind: $kind, plane: $plane)
}`

const semanticTraversalFixedQuery = `query SemanticTraversal($entry: semantic_graph_entry_input!, $direction: String!, $depth: Int!, $cut_id: String, $page_size: Int!, $page_token: String, $lens: String, $planes: [String!], $definitions: [semantic_graph_definition_ref_input!]) {
  semantic_traversal(entry: $entry, direction: $direction, depth: $depth, cut_id: $cut_id, page_size: $page_size, page_token: $page_token, lens: $lens, planes: $planes, definitions: $definitions)
}`

const semanticLensFixedQuery = `query SemanticLens($entry: semantic_graph_entry_input!, $direction: String!, $depth: Int!, $cut_id: String, $page_size: Int!, $page_token: String, $lens: String!) {
  semantic_lens(entry: $entry, direction: $direction, depth: $depth, cut_id: $cut_id, page_size: $page_size, page_token: $page_token, lens: $lens)
}`

const semanticElementCatalogFixedQuery = `query SemanticElementCatalog {
  semantic_element_catalog
}`

const semanticPVFixedQuery = `query SemanticPVCurrent($request: m2m_current_snapshot_request!) {
  semantic_pv_current(request: $request) { snapshot evaluation selections projection }
}`

const semanticStorageFixedQuery = `query SemanticStorageCurrent($request: m2m_current_snapshot_request!) {
  semantic_storage_current(request: $request) { snapshot evaluation selections projection }
}`

const semanticEVSEFixedQuery = `query SemanticEVSECurrent($request: m2m_current_snapshot_request!) {
  semantic_evse_current(request: $request) { snapshot evaluation selections projection }
}`

// Config supplies the one immutable SemReg evaluation used by every public PV
// consumer. The legacy canonical-PV provider is deliberately absent.
type Config struct {
	AllowedAssets          map[string]struct{}
	MonotonicMilliseconds  func() int64
	SemanticPVCurrent      func(context.Context, string) (json.RawMessage, bool)
	SemanticStorageCurrent func(context.Context, string) (json.RawMessage, bool)
	SemanticEVSECurrent    func(context.Context, string) (json.RawMessage, bool)

	// The four graph reads. Each provider receives the document's variables
	// and returns the canonical record the local schema returns for the same
	// request. An error carrying an Extensions() map with a "code" entry is
	// answered with that stable code; any other error is SOURCE_UNAVAILABLE.
	SemanticResources      GraphRead
	SemanticTraversal      GraphRead
	SemanticLens           GraphRead
	SemanticElementCatalog GraphRead
}

// GraphRead serves one graph read from its variables.
type GraphRead func(context.Context, map[string]any) (json.RawMessage, error)

type graphDocument struct {
	root     string
	query    string
	required []string
	optional []string
	kinds    map[string]string
}

var graphDocuments = map[string]graphDocument{
	"SemanticResources": {root: "semantic_resources", query: semanticResourcesFixedQuery,
		required: []string{"page_size"}, optional: []string{"cut_id", "page_token", "kind", "plane"},
		kinds: map[string]string{"cut_id": "string", "page_token": "string", "kind": "string", "plane": "string", "page_size": "int"}},
	"SemanticTraversal": {root: "semantic_traversal", query: semanticTraversalFixedQuery,
		required: []string{"entry", "direction", "depth", "page_size"}, optional: []string{"cut_id", "page_token", "lens", "planes", "definitions"},
		kinds: map[string]string{"entry": "entry", "direction": "string", "depth": "int", "page_size": "int", "cut_id": "string", "page_token": "string", "lens": "string", "planes": "strings", "definitions": "definitions"}},
	"SemanticLens": {root: "semantic_lens", query: semanticLensFixedQuery,
		required: []string{"entry", "direction", "depth", "page_size", "lens"}, optional: []string{"cut_id", "page_token"},
		kinds: map[string]string{"entry": "entry", "direction": "string", "depth": "int", "page_size": "int", "cut_id": "string", "page_token": "string", "lens": "string"}},
	"SemanticElementCatalog": {root: "semantic_element_catalog", query: semanticElementCatalogFixedQuery},
}

type handler struct {
	cfg               Config
	queryShape        string
	storageQueryShape string
	evseQueryShape    string
	graphShapes       map[string]string
	mu                sync.Mutex
	limits            map[string]*list.Element // value: *requestLimit
	lru               *list.List               // front = most recently used
	limitCapacity     int
}

type requestLimit struct {
	key      string
	inFlight bool
	tokens   int
	at       int64
	lastUsed int64
}

type rateLimitKey struct{}

// WithRateLimitKey lets an in-process caller choose the request-budget
// partition. It carries no identity and grants no access. Requests from the
// network listener never carry it and are partitioned by remote host.
func WithRateLimitKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, rateLimitKey{}, key)
}

// WithMTLSPrincipal is a compatibility alias of WithRateLimitKey kept for
// in-process callers until the M2M cleanup removes it; despite its name, no
// client principal is derived or required.
func WithMTLSPrincipal(ctx context.Context, key string) context.Context {
	return WithRateLimitKey(ctx, key)
}

// requestRateLimitKey partitions the request budget by the remote host (the
// port is ignored so reconnecting does not refill the budget), unless an
// in-process caller supplied an explicit partition.
func requestRateLimitKey(r *http.Request) string {
	if key, ok := r.Context().Value(rateLimitKey{}).(string); ok && strings.TrimSpace(key) != "" {
		return "key:" + key
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "remote:" + host
}

func NewHandler(cfg Config) (http.Handler, error) {
	if cfg.SemanticPVCurrent == nil && cfg.SemanticStorageCurrent == nil && cfg.SemanticEVSECurrent == nil &&
		cfg.SemanticResources == nil && cfg.SemanticTraversal == nil && cfg.SemanticLens == nil && cfg.SemanticElementCatalog == nil {
		return nil, errors.New("an authoritative SemReg projection provider is required")
	}
	if cfg.AllowedAssets == nil {
		cfg.AllowedAssets = map[string]struct{}{}
	}
	if cfg.MonotonicMilliseconds == nil {
		started := time.Now()
		cfg.MonotonicMilliseconds = func() int64 { return time.Since(started).Milliseconds() }
	}
	shape, _, _, err := queryDocumentShape(semanticPVFixedQuery)
	if err != nil {
		return nil, errors.New("invalid embedded SemReg PV query")
	}
	storageShape, _, _, err := queryDocumentShape(semanticStorageFixedQuery)
	if err != nil {
		return nil, errors.New("invalid embedded SemReg storage query")
	}
	evseShape, _, _, err := queryDocumentShape(semanticEVSEFixedQuery)
	if err != nil {
		return nil, errors.New("invalid embedded SemReg EVSE query")
	}
	graphShapes := map[string]string{}
	for name, document := range graphDocuments {
		documentShape, _, _, err := queryDocumentShape(document.query)
		if err != nil {
			return nil, errors.New("invalid embedded SemReg graph query")
		}
		graphShapes[name] = documentShape
	}
	return &handler{cfg: cfg, queryShape: shape, storageQueryShape: storageShape, evseQueryShape: evseShape, graphShapes: graphShapes, limits: make(map[string]*list.Element), lru: list.New(), limitCapacity: rateLimitTableCapacity}, nil
}

// ServeHTTP spends the remote host's request budget before any other work, so
// a refused host never has its body read or its document parsed.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.budgeted(http.HandlerFunc(h.serve)).ServeHTTP(w, r)
}

// RequestBudget wraps next so that every request it serves first spends the
// per-host request budget owned by budget, which must be a handler returned by
// NewHandler. The wrapped routes share that handler's bounded table, key
// derivation and refusal response, so one remote host has a single budget
// across the GraphQL route and every route wrapped here. A refused request is
// answered by refuse, which lets a wrapped route use its own protocol's error
// shape; a nil refuse writes the GraphQL refusal. It carries no identity and
// grants no access.
func RequestBudget(budget http.Handler, next http.Handler, refuse func(http.ResponseWriter)) (http.Handler, error) {
	owner, ok := budget.(*handler)
	if !ok || owner == nil {
		return nil, errors.New("request budget owner must be an M2M GraphQL handler")
	}
	if next == nil {
		return nil, errors.New("request budget requires a wrapped handler")
	}
	return owner.budgetedWith(next, refuse), nil
}

// budgeted admits the request before next runs and releases the entry on every
// exit path, including a panic unwinding through next.
func (h *handler) budgeted(next http.Handler) http.Handler {
	return h.budgetedWith(next, nil)
}

func (h *handler) budgetedWith(next http.Handler, refuse func(http.ResponseWriter)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := h.admit(requestRateLimitKey(r))
		if limit == nil {
			if refuse != nil {
				refuse(w)
				return
			}
			writeError(w, "", "REQUEST_LIMIT_EXCEEDED")
			return
		}
		defer h.release(limit)
		next.ServeHTTP(w, r)
	})
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, "", "REQUEST_INVALID")
		return
	}
	if r.URL.Path != route || r.URL.RawQuery != "" {
		writeError(w, "", "QUERY_REJECTED")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		writeError(w, "", "REQUEST_INVALID")
		return
	}
	if len(body) > maxRequestBytes {
		writeError(w, "", "REQUEST_LIMIT_EXCEEDED")
		return
	}
	if err := validateJSON(body); err != nil {
		writeError(w, "", "REQUEST_INVALID")
		return
	}
	if name, ok := graphOperationName(body); ok {
		h.serveGraph(w, r, body, name)
		return
	}
	request, err := decodeClosedRequest(body)
	if err != nil {
		writeError(w, "", "REQUEST_INVALID")
		return
	}
	queryShape, queryDepth, queryFields, err := queryDocumentShape(request.Query)
	if queryDepth > maxQueryDepth || queryFields > maxSelectedFields {
		writeError(w, "", "REQUEST_LIMIT_EXCEEDED")
		return
	}
	if err != nil {
		writeError(w, "", "QUERY_REJECTED")
		return
	}
	root, contract, provider := "", "", (func(context.Context, string) (json.RawMessage, bool))(nil)
	switch request.OperationName {
	case "SemanticPVCurrent":
		root, contract, provider = "semantic_pv_current", semanticPVContractID, h.cfg.SemanticPVCurrent
	case "SemanticStorageCurrent":
		root, contract, provider = "semantic_storage_current", semanticStorageContractID, h.cfg.SemanticStorageCurrent
	case "SemanticEVSECurrent":
		root, contract, provider = "semantic_evse_current", semanticEVSEContractID, h.cfg.SemanticEVSECurrent
	default:
		writeError(w, "", "QUERY_REJECTED")
		return
	}
	if (request.OperationName == "SemanticPVCurrent" && queryShape != h.queryShape) || (request.OperationName == "SemanticStorageCurrent" && queryShape != h.storageQueryShape) || (request.OperationName == "SemanticEVSECurrent" && queryShape != h.evseQueryShape) || provider == nil {
		writeError(w, root, "QUERY_REJECTED")
		return
	}
	if request.Variables.Request.ContractID != contract {
		writeError(w, root, "CONTRACT_INCOMPATIBLE")
		return
	}
	asset := request.Variables.Request.AssetRef
	if _, ok := h.cfg.AllowedAssets[asset]; !ok {
		writeError(w, root, "ASSET_FORBIDDEN")
		return
	}
	data, ok := provider(r.Context(), asset)
	if !ok || !json.Valid(data) {
		writeError(w, root, "SOURCE_UNAVAILABLE")
		return
	}
	var projection map[string]json.RawMessage
	if err := json.Unmarshal(data, &projection); err != nil || !hasExactKeys(projection, "snapshot", "evaluation", "selections", "projection") {
		writeError(w, root, "SOURCE_UNAVAILABLE")
		return
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{root: map[string]json.RawMessage{
		"snapshot": projection["snapshot"], "evaluation": projection["evaluation"], "selections": projection["selections"], "projection": projection["projection"],
	}}})
	if err != nil || len(encoded) > maxResponseBytes {
		writeError(w, root, "REQUEST_LIMIT_EXCEEDED")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// graphOperationName reports the operation name of a request that names one of
// the four graph reads.
func graphOperationName(body []byte) (string, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		return "", false
	}
	var name string
	if json.Unmarshal(envelope["operationName"], &name) != nil {
		return "", false
	}
	_, ok := graphDocuments[name]
	return name, ok
}

func (h *handler) graphProvider(name string) GraphRead {
	switch name {
	case "SemanticResources":
		return h.cfg.SemanticResources
	case "SemanticTraversal":
		return h.cfg.SemanticTraversal
	case "SemanticLens":
		return h.cfg.SemanticLens
	default:
		return h.cfg.SemanticElementCatalog
	}
}

func (h *handler) serveGraph(w http.ResponseWriter, r *http.Request, body []byte, name string) {
	document := graphDocuments[name]
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || !hasExactKeys(envelope, "operationName", "query", "variables") {
		writeError(w, "", "REQUEST_INVALID")
		return
	}
	var query string
	if err := json.Unmarshal(envelope["query"], &query); err != nil || query == "" {
		writeError(w, "", "REQUEST_INVALID")
		return
	}
	shape, depth, fields, err := queryDocumentShape(query)
	if depth > maxQueryDepth || fields > maxSelectedFields {
		writeError(w, "", "REQUEST_LIMIT_EXCEEDED")
		return
	}
	provider := h.graphProvider(name)
	if err != nil || shape != h.graphShapes[name] || provider == nil {
		writeError(w, document.root, "QUERY_REJECTED")
		return
	}
	variables, ok := decodeGraphVariables(envelope["variables"], document)
	if !ok {
		writeError(w, document.root, "REQUEST_INVALID")
		return
	}
	data, err := provider(r.Context(), variables)
	if err != nil {
		var coded interface{ Extensions() map[string]any }
		if errors.As(err, &coded) {
			if code, ok := coded.Extensions()["code"].(string); ok && code != "" {
				writeError(w, document.root, code)
				return
			}
		}
		writeError(w, document.root, "SOURCE_UNAVAILABLE")
		return
	}
	if !json.Valid(data) {
		writeError(w, document.root, "SOURCE_UNAVAILABLE")
		return
	}
	encoded, err := json.Marshal(map[string]any{"data": map[string]any{document.root: string(data)}})
	if err != nil || len(encoded) > maxResponseBytes {
		writeError(w, document.root, "REQUEST_LIMIT_EXCEEDED")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// decodeGraphVariables accepts only the variables the fixed document declares,
// with the declared types; a null optional variable is absent.
func decodeGraphVariables(raw json.RawMessage, document graphDocument) (map[string]any, bool) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var input map[string]any
	if err := decoder.Decode(&input); err != nil || input == nil {
		return nil, false
	}
	out := map[string]any{}
	for key, value := range input {
		kind, declared := document.kinds[key]
		if !declared {
			return nil, false
		}
		if value == nil {
			continue
		}
		converted, ok := convertGraphVariable(kind, value)
		if !ok {
			return nil, false
		}
		out[key] = converted
	}
	for _, key := range document.required {
		if _, ok := out[key]; !ok {
			return nil, false
		}
	}
	return out, true
}

func convertGraphVariable(kind string, value any) (any, bool) {
	switch kind {
	case "string":
		text, ok := value.(string)
		return text, ok
	case "int":
		number, ok := value.(json.Number)
		if !ok {
			return nil, false
		}
		n, err := number.Int64()
		if err != nil || n < -1<<31 || n > 1<<31-1 {
			return nil, false
		}
		return int(n), true
	case "strings":
		list, ok := value.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, 0, len(list))
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return nil, false
			}
			out = append(out, item)
		}
		return out, true
	case "entry":
		return convertGraphObject(value, map[string]bool{"kind": true}, "kind", "resource", "lens")
	case "definitions":
		list, ok := value.([]any)
		if !ok {
			return nil, false
		}
		out := make([]any, 0, len(list))
		for _, item := range list {
			converted, ok := convertGraphObject(item, map[string]bool{"id": true, "version": true, "pack": true}, "id", "version", "pack")
			if !ok {
				return nil, false
			}
			pack, ok := convertGraphObject(converted.(map[string]any)["pack"], map[string]bool{"id": true, "version": true}, "id", "version")
			if !ok {
				return nil, false
			}
			converted.(map[string]any)["pack"] = pack
			out = append(out, converted)
		}
		return out, true
	}
	return nil, false
}

// convertGraphObject accepts an object limited to the named members; required
// members must be present. Members other than "pack" are strings.
func convertGraphObject(value any, required map[string]bool, members ...string) (any, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	allowed := map[string]bool{}
	for _, member := range members {
		allowed[member] = true
	}
	out := map[string]any{}
	for key, item := range object {
		if !allowed[key] {
			return nil, false
		}
		if key == "pack" {
			out[key] = item
			continue
		}
		if item == nil {
			continue
		}
		if _, ok := item.(string); !ok {
			return nil, false
		}
		out[key] = item
	}
	for key := range required {
		if _, ok := out[key]; !ok {
			return nil, false
		}
	}
	return out, true
}

// admit returns the admitted budget entry, or nil when the key's budget is
// exhausted or a request for the key is already in flight.
func (h *handler) admit(key string) *requestLimit {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.cfg.MonotonicMilliseconds()
	h.evictIdleLocked(now)
	var limit *requestLimit
	if element := h.limits[key]; element != nil {
		limit = element.Value.(*requestLimit)
		h.lru.MoveToFront(element)
	} else {
		for h.lru.Len() >= h.limitCapacity {
			victim := h.lruVictimLocked()
			if victim == nil {
				// Every entry is in flight: exceed capacity rather than
				// drop a guard or refuse the new key.
				break
			}
			h.removeLocked(victim)
		}
		limit = &requestLimit{key: key, tokens: rateLimitBurst, at: now}
		h.limits[key] = h.lru.PushFront(limit)
	}
	limit.lastUsed = now
	if now > limit.at {
		elapsedIntervals := (now - limit.at) / rateLimitRefillMillis
		limit.tokens = min(rateLimitBurst, limit.tokens+int(elapsedIntervals))
		limit.at += elapsedIntervals * rateLimitRefillMillis
	}
	if limit.inFlight || limit.tokens == 0 {
		return nil
	}
	limit.inFlight = true
	limit.tokens--
	return limit
}

// release clears the in-flight mark on the entry admit returned. In-flight
// entries are never evicted; the entry is still addressed by pointer so that a
// release can never clear or touch a different entry for the same key.
func (h *handler) release(limit *requestLimit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	limit.inFlight = false
	if element := h.limits[limit.key]; element != nil && element.Value == limit {
		limit.lastUsed = h.cfg.MonotonicMilliseconds()
		h.lru.MoveToFront(element)
	}
}

// evictIdleLocked drops entries untouched for longer than the idle window,
// skipping entries with a request in flight. The list is ordered by last use,
// so expired entries sit at the back and the sweep costs O(1) amortised per
// admission plus one step per in-flight entry passed over.
func (h *handler) evictIdleLocked(now int64) {
	for element := h.lru.Back(); element != nil; {
		limit := element.Value.(*requestLimit)
		if now-limit.lastUsed <= rateLimitIdleMillis {
			return
		}
		prev := element.Prev()
		if !limit.inFlight {
			h.removeLocked(element)
		}
		element = prev
	}
}

// lruVictimLocked returns the least recently used entry without a request in
// flight, or nil when every entry is in flight.
func (h *handler) lruVictimLocked() *list.Element {
	for element := h.lru.Back(); element != nil; element = element.Prev() {
		if !element.Value.(*requestLimit).inFlight {
			return element
		}
	}
	return nil
}

func (h *handler) removeLocked(element *list.Element) {
	limit := h.lru.Remove(element).(*requestLimit)
	delete(h.limits, limit.key)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func writeError(w http.ResponseWriter, root, code string) {
	path := []string{}
	if root != "" {
		path = []string{root}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []any{map[string]any{"message": "M2M request failed", "path": path, "extensions": map[string]string{"code": code}}}})
}

func validateJSON(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := walkJSON(decoder, 0); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("extra JSON value")
	}
	_, err := decoder.Token()
	if err != io.EOF {
		return errors.New("extra JSON value")
	}
	return nil
}

type closedRequest struct {
	OperationName string
	Query         string
	Variables     struct {
		Request struct {
			ContractID string
			AssetRef   string
		}
	}
}

func decodeClosedRequest(data []byte) (closedRequest, error) {
	var request closedRequest
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || !hasExactKeys(root, "operationName", "query", "variables") {
		return request, errors.New("invalid request envelope")
	}
	if err := json.Unmarshal(root["operationName"], &request.OperationName); err != nil || request.OperationName == "" {
		return request, errors.New("invalid operation name")
	}
	if err := json.Unmarshal(root["query"], &request.Query); err != nil || request.Query == "" {
		return request, errors.New("invalid query")
	}
	var variables map[string]json.RawMessage
	if err := json.Unmarshal(root["variables"], &variables); err != nil || !hasExactKeys(variables, "request") {
		return request, errors.New("invalid variables")
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(variables["request"], &input); err != nil || !hasExactKeys(input, "contract_id", "asset_ref") {
		return request, errors.New("invalid request input")
	}
	if err := json.Unmarshal(input["contract_id"], &request.Variables.Request.ContractID); err != nil {
		return request, errors.New("invalid contract ID")
	}
	if err := json.Unmarshal(input["asset_ref"], &request.Variables.Request.AssetRef); err != nil {
		return request, errors.New("invalid asset reference")
	}
	return request, nil
}

func hasExactKeys(values map[string]json.RawMessage, expected ...string) bool {
	if len(values) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := values[key]; !ok {
			return false
		}
	}
	return true
}

func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("object key")
				}
				if _, duplicate := seen[name]; duplicate {
					return errors.New("duplicate key")
				}
				seen[name] = struct{}{}
				if err := walkJSON(decoder, depth+1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walkJSON(decoder, depth+1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = decoder.Token()
		return err
	default:
		return nil
	}
}

func queryDocumentShape(query string) (string, int, int, error) {
	document, err := parser.Parse(parser.ParseParams{Source: query, Options: parser.ParseOptions{NoLocation: true, NoSource: true}})
	if err != nil {
		return "", 0, 0, err
	}
	maximumDepth, selectedFields := 0, 0
	var walk func(*ast.SelectionSet, int)
	walk = func(selectionSet *ast.SelectionSet, parentDepth int) {
		if selectionSet == nil {
			return
		}
		for _, selection := range selectionSet.Selections {
			depth := parentDepth + 1
			if depth > maximumDepth {
				maximumDepth = depth
			}
			if _, ok := selection.(*ast.Field); ok {
				selectedFields++
			}
			walk(selection.GetSelectionSet(), depth)
		}
	}
	for _, node := range document.Definitions {
		if definition, ok := node.(ast.Definition); ok {
			walk(definition.GetSelectionSet(), 0)
		}
	}
	printed, ok := printer.Print(document).(string)
	if !ok {
		return "", maximumDepth, selectedFields, fmt.Errorf("unexpected printed query type")
	}
	return printed, maximumDepth, selectedFields, nil
}
