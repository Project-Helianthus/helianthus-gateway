package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-gateway/internal/portalconfig"
	"github.com/Project-Helianthus/helianthus-gateway/internal/semhub"
	"github.com/Project-Helianthus/helianthus-gateway/portal"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/identity"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/packs/infrastructure"
)

// pcjFactory plans two synthetic transports. It names a family and no model,
// and it is not a driver constructor package.
type pcjFactory struct{}

func (pcjFactory) Descriptor() driver.Descriptor { return driver.Descriptor{} }

func (pcjFactory) Plan(config driver.Config) ([]driver.InstancePlan, error) {
	if !config.Enabled {
		return nil, nil
	}
	return []driver.InstancePlan{{
		Driver:   "family.synthetic",
		Instance: driver.InstanceKey(config.Instance),
		Transports: []driver.TransportClaim{{
			Provider: "provider.synthetic", ConfigKey: "config." + config.Instance,
			AccessDomain: "domain.synthetic", Share: driver.ShareSerialized,
		}},
	}}, nil
}

func (pcjFactory) Construct(context.Context, driver.InstancePlan) (driver.Instance, error) {
	return pcjInstance{}, nil
}

type pcjInstance struct{}

func (pcjInstance) Start(context.Context, driver.Session) error { return nil }
func (pcjInstance) FenceWithdrawal()                            {}
func (pcjInstance) Stop(context.Context) driver.CloseReport {
	return driver.CloseReport{Confirmed: true}
}

func pcjInstances() []gatewayHostInstance {
	return []gatewayHostInstance{
		{Kind: "family.synthetic", Instance: "member.one", Enabled: true},
		{Kind: "family.synthetic", Instance: "member.two", Enabled: true},
	}
}

type pcjView struct {
	Identity identity.Resolution
	Links    []identity.Link
	Relays   []runtime.PlannedLink
}

type pcjFile struct {
	Format   string                        `json:"format"`
	Revision string                        `json:"revision"`
	Records  []portalconfig.OperatorRecord `json:"records"`
	Joins    []portalconfig.CertifiedJoin  `json:"certified_joins,omitempty"`
}

func pcjWrite(t *testing.T, root, revision string, records []portalconfig.OperatorRecord, joins []portalconfig.CertifiedJoin) {
	t.Helper()
	if records == nil {
		records = []portalconfig.OperatorRecord{}
	}
	raw, err := json.Marshal(pcjFile{Format: portalconfig.OperatorRecordsFormat, Revision: revision, Records: records, Joins: joins})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, portalconfig.OperatorRecordsFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pcjNode(ref driver.NodeRef) portalconfig.CertifiedNode {
	return portalconfig.CertifiedNode{Bridge: string(ref.Bridge), ID: string(ref.ID)}
}

func pcjStatement(action string, low, high, survivor driver.NodeRef, revision, target string) portalconfig.CertifiedJoin {
	return portalconfig.CertifiedJoin{Action: action, A: pcjNode(low), B: pcjNode(high), Survivor: pcjNode(survivor), Revision: revision, TargetRevision: target}
}

func pcjCopyLinks(in []identity.Link) []identity.Link {
	out := make([]identity.Link, len(in))
	for i, link := range in {
		out[i] = link
		out[i].Claims = make([]identity.LinkClaimView, len(link.Claims))
		for j, claim := range link.Claims {
			out[i].Claims[j] = claim
			out[i].Claims[j].Basis = append([]semreg.EvidenceRef(nil), claim.Basis...)
		}
	}
	return out
}

func pcjSortLinks(links []identity.Link) []identity.Link {
	out := pcjCopyLinks(links)
	for i := range out {
		sortClaims := out[i].Claims
		for a := 0; a < len(sortClaims); a++ {
			for b := a + 1; b < len(sortClaims); b++ {
				pa := string(sortClaims[a].Publisher.Driver) + "\x00" + string(sortClaims[a].Publisher.Instance)
				pb := string(sortClaims[b].Publisher.Driver) + "\x00" + string(sortClaims[b].Publisher.Instance)
				da, _ := json.Marshal(sortClaims[a].Declared)
				db, _ := json.Marshal(sortClaims[b].Declared)
				if pb < pa || (pa == pb && string(db) < string(da)) {
					sortClaims[a], sortClaims[b] = sortClaims[b], sortClaims[a]
				}
			}
		}
	}
	for a := 0; a < len(out); a++ {
		for b := a + 1; b < len(out); b++ {
			ka, _ := json.Marshal(out[a].Key)
			kb, _ := json.Marshal(out[b].Key)
			if string(kb) < string(ka) {
				out[a], out[b] = out[b], out[a]
			}
		}
	}
	return out
}

// pcjExpectMoved is the retained links after one rebind, computed from the
// links observed before that rebind. Declared claims stay. Endpoints that
// were the old member move to the rebind survivor, and claims of one
// effective key are grouped.
func pcjExpectMoved(before []identity.Link, old, survivor driver.NodeRef) []identity.Link {
	type group struct {
		key    driver.LinkKey
		claims []identity.LinkClaimView
	}
	groups := map[string]*group{}
	var order []string
	for _, link := range before {
		for _, claim := range link.Claims {
			effective := claim.Declared
			if effective.From == old {
				effective.From = survivor
			}
			if effective.To == old {
				effective.To = survivor
			}
			raw, _ := json.Marshal(effective)
			text := string(raw)
			if groups[text] == nil {
				groups[text] = &group{key: effective}
				order = append(order, text)
			}
			groups[text].claims = append(groups[text].claims, identity.LinkClaimView{
				Publisher: claim.Publisher, Declared: claim.Declared, Basis: append([]semreg.EvidenceRef(nil), claim.Basis...),
			})
		}
	}
	out := make([]identity.Link, 0, len(order))
	for _, text := range order {
		out = append(out, identity.Link{Key: groups[text].key, Claims: groups[text].claims})
	}
	return pcjSortLinks(out)
}

func pcjTargets(relays []runtime.PlannedLink) []driver.NodeRef {
	seen := map[driver.NodeRef]bool{}
	var out []driver.NodeRef
	for _, relay := range relays {
		if relay.To == relay.From || seen[relay.To] {
			continue
		}
		seen[relay.To] = true
		out = append(out, relay.To)
	}
	for a := 0; a < len(out); a++ {
		for b := a + 1; b < len(out); b++ {
			if out[b].Bridge < out[a].Bridge || (out[b].Bridge == out[a].Bridge && out[b].ID < out[a].ID) {
				out[a], out[b] = out[b], out[a]
			}
		}
	}
	return out
}

func pcjJoined(res identity.Resolution, a, b driver.NodeRef) (identity.Resolved, bool) {
	for _, item := range res.Resources {
		hasA, hasB := false, false
		for _, member := range item.Members {
			hasA = hasA || member.Ref == a
			hasB = hasB || member.Ref == b
		}
		if hasA && hasB {
			return item, true
		}
	}
	return identity.Resolved{}, false
}

func pcjHolds(res identity.Resolution, ref driver.NodeRef) bool {
	for _, item := range res.Resources {
		for _, member := range item.Members {
			if member.Ref == ref {
				return true
			}
		}
	}
	return false
}

func pcjAbsent(res identity.Resolution, ref driver.NodeRef) bool {
	for _, finding := range res.Findings {
		if finding.Kind != identity.FindingMemberAbsent {
			continue
		}
		for _, member := range finding.Members {
			if member == ref {
				return true
			}
		}
	}
	return false
}

func pcjRequireSeparate(t *testing.T, res identity.Resolution, low, high driver.NodeRef) {
	t.Helper()
	if _, joined := pcjJoined(res, low, high); joined {
		t.Fatalf("the pair is joined without the exact certification: %+v", res)
	}
	if !pcjHolds(res, low) || !pcjHolds(res, high) || pcjAbsent(res, low) || pcjAbsent(res, high) {
		t.Fatalf("a member is not resolvable: %+v", res)
	}
}

func pcjRequireCertified(t *testing.T, res identity.Resolution, low, high driver.NodeRef) {
	t.Helper()
	joined, ok := pcjJoined(res, low, high)
	if !ok || joined.Representative != high || len(joined.Members) != 2 {
		t.Fatalf("resolution %+v, want representative %v over both members", res, high)
	}
	if pcjAbsent(res, low) || pcjAbsent(res, high) {
		t.Fatalf("a certified member is absent: %+v", res.Findings)
	}
}

// pcjRequireMoved checks the rebind against the links captured before it.
// A retained claim whose declared endpoint is the old member must be shown
// at the rebind survivor, with the declared key and basis kept. Planned
// relays stay on the allocated refs. A capture with no such claim has
// nothing for a rebind to move: the links stay as captured and the relays
// stay allocated. The frozen catalog stages no retained link on generic
// equipment, so that empty capture is the one this host produces.
func pcjRequireMoved(t *testing.T, before pcjView, after pcjView, old, survivor driver.NodeRef) {
	t.Helper()
	declared := false
	for _, link := range before.Links {
		for _, claim := range link.Claims {
			if claim.Declared.From == old || claim.Declared.To == old {
				declared = true
			}
		}
	}
	if !reflect.DeepEqual(after.Relays, before.Relays) {
		t.Fatalf("planned relays %+v, want the allocated routes %+v", after.Relays, before.Relays)
	}
	if !declared {
		if !reflect.DeepEqual(pcjSortLinks(after.Links), pcjSortLinks(before.Links)) {
			t.Fatalf("links changed with no retained claim on %v: before %+v after %+v", old, before.Links, after.Links)
		}
		t.Logf("no retained link is declared on %v; relays stayed on the allocated refs", old)
		return
	}
	want := pcjExpectMoved(before.Links, old, survivor)
	if reflect.DeepEqual(pcjSortLinks(before.Links), want) {
		t.Fatal("the rebind did not move a retained link")
	}
	if !reflect.DeepEqual(pcjSortLinks(after.Links), want) {
		t.Fatalf("links %+v, want the endpoints of %v moved to %v with the declared claims kept: %+v", after.Links, old, survivor, want)
	}
}

func pcjCapture(t *testing.T, opened *gatewayHost) pcjView {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cut, err := opened.site.Runtime().Cut(ctx)
	if err != nil {
		t.Fatal(err)
	}
	view := pcjView{Identity: cut.Identity, Links: pcjCopyLinks(cut.Links), Relays: append([]runtime.PlannedLink(nil), cut.Relays...)}
	if err = cut.Close(); err != nil {
		t.Fatal(err)
	}
	return view
}

// TestTrustedCertifiedJoinActionsInComposedHost drives the real Portal
// handler, the existing configuration store and composed host startup from a
// trusted local directory.
func TestTrustedCertifiedJoinActionsInComposedHost(t *testing.T) {
	clearGatewayTrustedJoins()
	t.Cleanup(clearGatewayTrustedJoins)
	root := t.TempDir()
	state := t.TempDir()
	stateFile := filepath.Join(state, "runtime_state.json")

	var portalRuntime *gatewayPortalConfigRuntime
	openPortal := func() {
		t.Helper()
		if portalRuntime != nil {
			if err := portalRuntime.Close(); err != nil {
				t.Fatal(err)
			}
			portalRuntime = nil
		}
		cfg := ebusgateway.DefaultConfig()
		cfg.PortalConfigRoot = root
		portalRuntime = openGatewayPortalConfigRuntime(cfg)
	}
	t.Cleanup(func() {
		if portalRuntime != nil {
			_ = portalRuntime.Close()
		}
	})

	var opened *gatewayHost
	closeHost := func() {
		t.Helper()
		if opened == nil {
			return
		}
		if err := opened.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		opened = nil
	}
	openHost := func() pcjView {
		t.Helper()
		closeHost()
		var err error
		opened, err = openGatewayHost(context.Background(), stateFile, pcjInstances(), pcjFactory{})
		if err != nil {
			t.Fatal(err)
		}
		if err = opened.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return pcjCapture(t, opened)
	}
	t.Cleanup(func() {
		_ = func() error {
			if opened != nil {
				return opened.Close(context.Background())
			}
			return nil
		}()
	})
	refuseHost := func() error {
		t.Helper()
		closeHost()
		candidate, err := openGatewayHost(context.Background(), stateFile, pcjInstances(), pcjFactory{})
		if err != nil {
			return err
		}
		_ = candidate.Close(context.Background())
		return nil
	}

	discovered := openHost()
	targets := pcjTargets(discovered.Relays)
	if len(targets) != 2 {
		t.Fatalf("relay targets %+v from %+v, want the two link nodes", targets, discovered.Relays)
	}
	low, high := targets[0], targets[1]
	pcjRequireSeparate(t, discovered.Identity, low, high)
	closeHost()

	certify := pcjStatement(portalconfig.CertifiedJoinCertify, low, high, high, "1", "")
	rebind := pcjStatement(portalconfig.CertifiedJoinRebind, low, high, low, "2", "")
	revoke := pcjStatement(portalconfig.CertifiedJoinRevoke, low, high, high, "1", "1")
	pcjWrite(t, root, "0", nil, []portalconfig.CertifiedJoin{certify})
	raw, err := os.ReadFile(filepath.Join(root, portalconfig.OperatorRecordsFile))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, portalconfig.OperatorRecordsFile), []byte(strings.Replace(string(raw), `"revision":"1"`, `"revision":"1","digest":"present"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = portalconfig.LoadTrustedCertifiedJoins(root); err == nil {
		t.Fatal("a presented digest was accepted as origin")
	}
	openPortal()
	unjoined := openHost()
	pcjRequireSeparate(t, unjoined.Identity, low, high)
	closeHost()
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil

	site := oraSitePath()
	site.unlinked = true
	native := site.native()
	linkRecord := portalconfig.OperatorRecord{
		Form: portalconfig.OperatorFormIdentityLink, Home: string(oraSiteAsset), ResourceID: string(oraConnection),
		BasisClass: string(semreg.RelationConfiguration), Native: &native,
	}
	pcjWrite(t, root, "0", []portalconfig.OperatorRecord{linkRecord}, []portalconfig.CertifiedJoin{certify})
	hub, err := semhub.New(semhub.Options{})
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := operatorPublisherFor(hub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = hub.Declare(semhub.AssetSpec{AssetID: oraSiteAsset, SourceID: oraSiteSource, Driver: oraSiteDriver, Validators: []semreg.PackValidator{infrastructure.New()}, Graph: true, Contributors: []semhub.ContributorSpec{publisher.Contributor(infrastructure.New())}}); err != nil {
		t.Fatal(err)
	}
	if site.handle, err = hub.AdmitSource(semhub.Admission{Source: oraSiteSource, Driver: oraSiteDriver}); err != nil {
		t.Fatal(err)
	}
	fixture := &oraFixture{hub: hub, start: time.Now()}
	if err = fixture.publish(t, site, nil); err != nil {
		t.Fatal(err)
	}
	openPortal()
	cfg := ebusgateway.DefaultConfig()
	cfg.PortalConfigRoot = root
	callbacks := portalRuntime.callbacks(cfg, &gatewayRuntimeControl{hub: hub})
	fixture.records = callbacks.OperatorRecords
	stored, err := fixture.records.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opr27Await(t, fixture.records, stored, []string{"waiting:" + portalOperatorIdentityLinkPending})
	audit := portalOperatorAudit(oraSiteAsset, stored.Records)
	before := fixture.graphFact(t, oraSiteAsset)
	if len(before.services) != 0 || len(before.capabilities) != 0 || len(before.relations) != 0 {
		t.Fatalf("route authority before the configured link: %+v", before)
	}
	carried := func() (semreg.IdentityLink, bool) {
		for _, link := range fixture.inspect(t, oraSiteAsset).Snapshot.IdentityLinks {
			if link.BindingID == site.binding(t) && link.Resource != nil {
				return link, true
			}
		}
		return semreg.IdentityLink{}, false
	}
	if link, ok := carried(); ok {
		t.Fatalf("the configured link was published before the owner's batch: %+v", link)
	}
	if err = fixture.publish(t, site, nil); err != nil {
		t.Fatal(err)
	}
	if link, ok := carried(); ok {
		t.Fatalf("a batch that did not carry the configured link published it: %+v", link)
	}
	if err = fixture.publish(t, site, func(tx *semhub.Tx, batch *semreg.PublicationBatch) {
		batch.IdentityLinkUpserts = tx.OperatorLinks(batch.BindingUpserts)
	}); err != nil {
		t.Fatal(err)
	}
	link, ok := carried()
	if !ok || *link.Resource != oraConnection || link.State != semreg.LinkQualified || !reflect.DeepEqual(link.Basis, []semreg.EvidenceRef{audit.evidence()}) {
		t.Fatalf("link=%+v present=%v, want the configured resource and the audit of the stored records", link, ok)
	}
	if got := fixture.graphFact(t, oraSiteAsset); !reflect.DeepEqual(got, before) {
		t.Fatal("the configured link changed graph membership or route authority")
	}

	forbidden := []string{"certified_joins", "target_revision", string(low.ID), string(high.ID), "configuration.certified_join"}
	handler := portal.NewHandler(portal.Options{Config: callbacks})
	ask := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, target, strings.NewReader(body)))
		for _, token := range forbidden {
			if strings.Contains(response.Body.String(), token) {
				t.Fatalf("%s %s exported %q", method, target, token)
			}
		}
		return response
	}
	type pcjConfig struct {
		Desired             json.RawMessage `json:"desired"`
		ListenerPlan        json.RawMessage `json:"listener_plan"`
		ListenerApplication string          `json:"listener_application"`
		State               struct {
			Drivers []struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"drivers"`
		} `json:"state"`
		OperatorRecords json.RawMessage `json:"operator_records"`
		Historical      bool            `json:"historical"`
		Error           *struct {
			Code  string `json:"code"`
			Field string `json:"field"`
		} `json:"error"`
	}
	decode := func(response *httptest.ResponseRecorder) pcjConfig {
		t.Helper()
		var view pcjConfig
		if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	general := ask(http.MethodGet, "/api/v1/config", "")
	view := decode(general)
	if general.Code != http.StatusOK || len(view.Desired) == 0 || string(view.Desired) == "null" || len(view.ListenerPlan) == 0 || view.ListenerApplication != "pending" || len(view.State.Drivers) == 0 || len(view.OperatorRecords) == 0 {
		t.Fatalf("configuration view %d %+v", general.Code, view)
	}
	for _, driverState := range view.State.Drivers {
		if driverState.Status != "withheld" || driverState.Reason != "evidence_unavailable" {
			t.Fatalf("driver state %+v", view.State.Drivers)
		}
	}
	section := ask(http.MethodGet, "/api/v1/config?section=operator_records", "")
	sectionView := decode(section)
	if section.Code != http.StatusOK || !strings.Contains(section.Body.String(), `"form":"identity_link"`) || sectionView.Historical || sectionView.Error != nil {
		t.Fatalf("operator-record view %d %s", section.Code, section.Body.String())
	}
	unknown := ask(http.MethodGet, "/api/v1/config?section=drivers", "")
	unknownView := decode(unknown)
	if unknown.Code != http.StatusBadRequest || unknownView.Historical || unknownView.Error == nil || unknownView.Error.Code != "invalid_record" || unknownView.Error.Field != "section" || string(unknownView.Desired) != "null" {
		t.Fatalf("unknown section %d %+v", unknown.Code, unknownView)
	}
	historical := ask(http.MethodPut, "/api/v1/config?section=operator_records", "{}")
	historicalView := decode(historical)
	if historical.Code != http.StatusBadRequest || !historicalView.Historical || historicalView.Error == nil || historicalView.Error.Code != "invalid_record" || len(historicalView.Desired) == 0 {
		t.Fatalf("historical refusal %d %+v", historical.Code, historicalView)
	}
	accepted, err := callbacks.OperatorRecords.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	injected, err := json.Marshal(accepted)
	if err != nil {
		t.Fatal(err)
	}
	injectedBody := strings.Replace(string(injected), `"records":`, `"certified_joins":[{"action":"certify","a":{"bridge":"`+string(low.Bridge)+`","id":"`+string(low.ID)+`"},"b":{"bridge":"`+string(high.Bridge)+`","id":"`+string(high.ID)+`"},"survivor":{"bridge":"`+string(high.Bridge)+`","id":"`+string(high.ID)+`"},"revision":"1","target_revision":"1"}],"records":`, 1)
	rejected := ask(http.MethodPut, "/api/v1/config?section=operator_records", injectedBody)
	rejectedView := decode(rejected)
	if rejected.Code != http.StatusBadRequest || !rejectedView.Historical || rejectedView.Error == nil || rejectedView.Error.Code != "invalid_record" {
		t.Fatalf("trusted fields in an ordinary replacement %d %+v", rejected.Code, rejectedView)
	}
	changed := accepted
	changed.Records[0].ResourceID = "resource:ora:connection-2"
	changedBody, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	changedResponse := ask(http.MethodPut, "/api/v1/config?section=operator_records", string(changedBody))
	if changedResponse.Code != http.StatusOK {
		t.Fatalf("ordinary change %d %s", changedResponse.Code, changedResponse.Body.String())
	}
	evidence := portalconfig.CertifiedJoinEvidence(certify)
	if loaded, err := portalconfig.LoadTrustedCertifiedJoins(root); err != nil || len(loaded) != 1 || loaded[0].Join != certify || loaded[0].Evidence != evidence {
		t.Fatalf("ordinary change rewrote the host-local section: %+v %v", loaded, err)
	}
	var failed *httptest.ResponseRecorder
	func() {
		if chmodErr := os.Chmod(root, 0o555); chmodErr != nil {
			t.Fatal(chmodErr)
		}
		defer func() { _ = os.Chmod(root, 0o755) }()
		failedBody, marshalErr := json.Marshal(portalconfig.OperatorRecordSet{Format: portalconfig.OperatorRecordsFormat, Revision: "1", Records: []portalconfig.OperatorRecord{linkRecord}})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		failed = ask(http.MethodPut, "/api/v1/config?section=operator_records", string(failedBody))
	}()
	failedView := decode(failed)
	if failed.Code != http.StatusServiceUnavailable || !failedView.Historical || failedView.Error == nil || failedView.Error.Code != "persistence_failed" {
		t.Fatalf("failed replacement %d %+v %s", failed.Code, failedView, failed.Body.String())
	}
	if loaded, err := portalconfig.LoadTrustedCertifiedJoins(root); err != nil || len(loaded) != 1 || loaded[0].Join != certify || loaded[0].Evidence != evidence {
		t.Fatalf("failed replacement rewrote the host-local section: %+v %v", loaded, err)
	}
	clearedBody, err := json.Marshal(portalconfig.OperatorRecordSet{Format: portalconfig.OperatorRecordsFormat, Revision: "1", Records: []portalconfig.OperatorRecord{}})
	if err != nil {
		t.Fatal(err)
	}
	cleared := ask(http.MethodPut, "/api/v1/config?section=operator_records", string(clearedBody))
	if cleared.Code != http.StatusOK || strings.Contains(cleared.Body.String(), "identity_link") {
		t.Fatalf("clear %d %s", cleared.Code, cleared.Body.String())
	}
	if loaded, err := portalconfig.LoadTrustedCertifiedJoins(root); err != nil || len(loaded) != 1 || loaded[0].Join != certify || loaded[0].Evidence != evidence {
		t.Fatalf("clearing ordinary records rewrote the host-local section: %+v %v", loaded, err)
	}
	omitted := ask(http.MethodGet, "/api/v1/config", "")
	if omitted.Code != http.StatusOK || strings.Contains(omitted.Body.String(), "operator_records") {
		t.Fatalf("an empty ordinary section stayed in the configuration view: %s", omitted.Body.String())
	}

	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify})
	openPortal()
	certified := openHost()
	pcjRequireCertified(t, certified.Identity, low, high)
	if !reflect.DeepEqual(certified.Relays, discovered.Relays) {
		t.Fatalf("certify changed the planned relays: %+v", certified.Relays)
	}
	closeHost()

	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify, pcjStatement(portalconfig.CertifiedJoinRevoke, low, high, high, "1", "2")})
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	openPortal()
	if err = refuseHost(); !errors.Is(err, runtime.ErrStatementRevision) {
		t.Fatalf("wrong target revision: %v", err)
	}
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify})
	openPortal()
	if kept := openHost(); kept.Identity.Resources == nil {
		t.Fatal("reopen after a refused revocation returned no resolution")
	} else {
		pcjRequireCertified(t, kept.Identity, low, high)
	}
	closeHost()

	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify, pcjStatement(portalconfig.CertifiedJoinRevoke, low, high, high, "9", "9")})
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	openPortal()
	if err = refuseHost(); !errors.Is(err, identity.ErrWrongStatement) {
		t.Fatalf("wrong statement: %v", err)
	}
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify})
	openPortal()
	pcjRequireCertified(t, openHost().Identity, low, high)
	closeHost()

	expectedLinks := pcjExpectMoved(certified.Links, high, low)
	expectedRoutes := append([]runtime.PlannedLink(nil), certified.Relays...)
	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify, rebind})
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	openPortal()
	rebound := openHost()
	pcjRequireCertified(t, rebound.Identity, low, high)
	if !reflect.DeepEqual(pcjSortLinks(rebound.Links), expectedLinks) || !reflect.DeepEqual(rebound.Relays, expectedRoutes) {
		pcjRequireMoved(t, certified, rebound, high, low)
	}
	pcjRequireMoved(t, certified, rebound, high, low)
	closeHost()

	pcjWrite(t, root, "2", nil, []portalconfig.CertifiedJoin{certify, rebind, revoke})
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	openPortal()
	revoked := openHost()
	pcjRequireSeparate(t, revoked.Identity, low, high)
	if !reflect.DeepEqual(pcjSortLinks(revoked.Links), expectedLinks) || !reflect.DeepEqual(revoked.Relays, expectedRoutes) {
		t.Fatalf("revoking the certification moved the rebind: links %+v relays %+v", revoked.Links, revoked.Relays)
	}
	closeHost()
	if err = portalRuntime.Close(); err != nil {
		t.Fatal(err)
	}
	portalRuntime = nil
	clearGatewayTrustedJoins()
	restarted := openHost()
	pcjRequireSeparate(t, restarted.Identity, low, high)
	if !reflect.DeepEqual(pcjSortLinks(restarted.Links), expectedLinks) {
		t.Fatalf("checkpoint restart did not keep the revocation and the rebind: %+v", restarted.Links)
	}
	closeHost()
	openPortal()
	again := openHost()
	pcjRequireSeparate(t, again.Identity, low, high)
	if !reflect.DeepEqual(pcjSortLinks(again.Links), expectedLinks) || !reflect.DeepEqual(again.Relays, expectedRoutes) {
		t.Fatalf("file restart did not keep the revocation: links %+v relays %+v", again.Links, again.Relays)
	}
	closeHost()
}
