package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
)

// namespaceCuts is an empty served cut: the namespace read needs a cut source
// and the points it reads do not depend on one.
type namespaceCuts struct{ catalog *graph.Catalog }

func (c namespaceCuts) Capture(context.Context, driver.Filter) (tree.CutView, error) {
	cut, err := graph.AssembleCut(nil, c.catalog, graph.CutComplete)
	if err != nil {
		return tree.CutView{}, err
	}
	store := graph.NewCutStore(nil)
	if err = store.Retain(cut, nil, c.catalog); err != nil {
		return tree.CutView{}, err
	}
	return tree.CutView{Ref: graph.CutRef{CutID: cut.ID(), Completeness: cut.Completeness(), Members: cut.Members()}, Cut: cut, Store: store, Catalog: c.catalog}, nil
}

func (namespaceCuts) Lookup(context.Context, graph.CutRef) (tree.CutView, error) {
	return tree.CutView{}, &semreg.Error{ID: semreg.RevisionConflict, Detail: "not retained"}
}

func (namespaceCuts) Resume(context.Context, driver.Cursor, driver.Filter) (driver.Changes, error) {
	return driver.Changes{}, nil
}

type namespaceLife struct{}

func (namespaceLife) State(tree.InstanceRef) (tree.InstanceState, error) {
	return tree.InstanceState{Name: "running", Generation: 1}, nil
}
func (namespaceLife) OnFence(func(tree.Fence)) func() { return func() {} }

type namespaceOperations struct{}

func (namespaceOperations) Prepare(context.Context, graph.CutRef, semreg.DefinitionRef, tree.Input) (tree.PortPrepared, error) {
	return tree.PortPrepared{}, nil
}
func (namespaceOperations) Call(context.Context, tree.PortPrepared, tree.CallOptions) (tree.PortReceipt, error) {
	return tree.PortReceipt{}, nil
}
func (namespaceOperations) Outcome(context.Context, tree.ReceiptID) (driver.OperationResult, error) {
	return driver.OperationResult{}, nil
}

// namespacePublished is one member a synthetic driver registers: the family it
// publishes under, its instance, the node kind that carries it, its retained
// observation and the unit and channel it declares.
type namespacePublished struct {
	namespace, instance, kind, point, unit, channel string
	encoded                                         string
	freshness                                       semreg.Freshness
	received                                        int64
	age                                             *semreg.Uint64
	absent, derived                                 bool
	readErr                                         error
}

func namespaceFixture(t *testing.T, published []namespacePublished) *tree.Tree {
	t.Helper()
	catalog, err := graph.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	runtimeTree, err := tree.New(tree.Config{
		Registry:   tree.RegistryConfig{Installation: "installation.test", Bridge: "bridge.test", Incarnation: "incarnation.test", Lifecycle: namespaceLife{}},
		Cuts:       namespaceCuts{catalog: catalog},
		Operations: namespaceOperations{},
	})
	if err != nil {
		t.Fatal(err)
	}
	reserved := map[string]map[string]bool{}
	for _, item := range published {
		if reserved[item.namespace] == nil {
			reserved[item.namespace] = map[string]bool{}
		}
		reserved[item.namespace][item.instance] = true
	}
	for namespace, instances := range reserved {
		var keys []driver.InstanceKey
		for instance := range instances {
			keys = append(keys, driver.InstanceKey(instance))
		}
		if err = runtimeTree.Registry.Reserve(driver.Namespace{Owner: driver.DriverKind("owner." + namespace), Key: driver.NamespaceID(namespace)}, keys); err != nil {
			t.Fatal(err)
		}
	}
	type nodes struct {
		registry *tree.InstanceRegistry
		device   map[string]driver.NodeRef
		root     driver.NodeRef
	}
	bound := map[string]*nodes{}
	for _, item := range published {
		id := item.namespace + "/" + item.instance
		entry := bound[id]
		if entry == nil {
			registry, err := runtimeTree.Registry.Bind(driver.NamespaceID(item.namespace), driver.InstanceKey(item.instance), 1)
			if err != nil {
				t.Fatal(err)
			}
			transport, err := registry.Node(driver.NodeSpec{Kind: "node.transport", Local: "transport.one", Contract: "contract.test"})
			if err != nil {
				t.Fatal(err)
			}
			entry = &nodes{registry: registry, device: map[string]driver.NodeRef{}, root: transport}
			bound[id] = entry
		}
		device, ok := entry.device[item.kind]
		if !ok {
			var err error
			parent := entry.root
			device, err = entry.registry.Node(driver.NodeSpec{Parent: &parent, Kind: semreg.DefinitionID(item.kind), Local: driver.NodeID("device." + item.kind), Contract: "contract.test"})
			if err != nil {
				t.Fatal(err)
			}
			entry.device[item.kind] = device
		}
		observation := tree.Observation{
			Encoded: []byte(item.encoded), Freshness: item.freshness, Age: item.age,
			Absent: item.absent, Derived: item.derived,
		}
		if item.received != 0 {
			observation.ReceivedAt = &semreg.TimePoint{UnixNanoseconds: semreg.Int64(strconv.FormatInt(item.received, 10)), ClockID: "clock.utc", UncertaintyNS: "0"}
		}
		spec := driver.MemberSpec{Local: driver.MemberID(item.point), Schema: "schema.number", Units: semreg.DefinitionID(item.unit), Mode: "retained", Scope: "scope.test", Channel: item.channel}
		if err := entry.registry.ObserveMember(device, spec, func(context.Context) (tree.Observation, error) {
			return observation, item.readErr
		}); err != nil {
			t.Fatal(err)
		}
	}
	return runtimeTree
}

// TestNativePointNamespaceComposedScrape publishes numeric members through two
// synthetic drivers' retained namespaces and scrapes the composed route. The
// points reach the exposition only through the tree: the metrics runtime is
// given no observation list. Expected lines are written from the member
// declarations and the unit conversions, never from the exposition.
//
// Every published field is varied and each line pins one production mutation:
// the family string (protocol label from the namespace), the instance
// (driver_instance label from the member key), the node kind (device label),
// the member local id (point label), the channel (index label present only
// when published), the unit (family and converted value) and the retained
// freshness (stale gives an age and no value).
func TestNativePointNamespaceComposedScrape(t *testing.T) {
	const received = int64(92 * time.Second)
	published := []namespacePublished{
		{namespace: "family.alpha", instance: "inst.a", kind: "node.outdoor", point: "water_pressure", unit: "unit.bar", channel: "2", encoded: "1.25", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.alpha", instance: "inst.b", kind: "node.outdoor", point: "water_pressure", unit: "unit.bar", channel: "2", encoded: "1.25", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "pack_temperature", unit: "unit.celsius", encoded: "20", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.beta", instance: "inst.c", kind: "node.battery", point: "fan_speed", unit: "unit.revolution_per_minute", encoded: "3000", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.alpha", instance: "inst.a", kind: "node.meter", point: "stale_power", unit: "unit.watt", encoded: "12.5", freshness: semreg.FreshnessStale, received: received},
		{namespace: "family.alpha", instance: "inst.a", kind: "node.meter", point: "text_point", unit: "unit.watt", encoded: "not a number", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.alpha", instance: "inst.a", kind: "node.meter", point: "undocumented_unit", unit: "unit.unlisted", encoded: "7", freshness: semreg.FreshnessFresh, received: received},
	}
	for i := 1; i <= 1025; i++ {
		published = append(published, namespacePublished{namespace: "family.alpha", instance: "inst.a", kind: "node.meter", point: "point-" + strconv.Itoa(i), unit: "unit.watt", encoded: strconv.Itoa(i), freshness: semreg.FreshnessFresh, received: received})
	}
	runtimeTree := namespaceFixture(t, published)
	metrics, err := newGatewaySemanticMetrics(nil, gatewaySemanticMetricRegistry())
	if err != nil {
		t.Fatal(err)
	}
	metrics.now = func() time.Time { return time.Unix(101, 0).UTC() }
	metrics.tree = runtimeTree
	body := metrics.text(context.Background())
	for _, line := range []string{
		`helianthus_native_pressure{device="node.outdoor",driver_instance="inst.a",index="2",point="water_pressure",protocol="family.alpha",unit="pascals"} 125000`,
		`helianthus_native_pressure{device="node.outdoor",driver_instance="inst.b",index="2",point="water_pressure",protocol="family.alpha",unit="pascals"} 125000`,
		`helianthus_native_point_age{device="node.outdoor",driver_instance="inst.a",index="2",point="water_pressure",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_point_age{device="node.outdoor",driver_instance="inst.b",index="2",point="water_pressure",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_temperature{device="node.battery",driver_instance="inst.c",point="pack_temperature",protocol="family.beta",unit="kelvin"} 293.15`,
		`helianthus_native_point_age{device="node.battery",driver_instance="inst.c",point="pack_temperature",protocol="family.beta",unit="seconds"} 9`,
		`helianthus_native_rotational_speed{device="node.battery",driver_instance="inst.c",point="fan_speed",protocol="family.beta",unit="hertz"} 50`,
		`helianthus_native_point_age{device="node.meter",driver_instance="inst.a",point="stale_power",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_power{device="node.meter",driver_instance="inst.a",point="point-1025",protocol="family.alpha",unit="watts"} 1025`,
		`helianthus_native_point_age{device="node.meter",driver_instance="inst.a",point="point-1025",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_render_overflow 0`,
	} {
		if !strings.Contains(body, line+"\n") {
			t.Fatalf("missing %s", line)
		}
	}
	if strings.Count(body, "helianthus_native_power{") != 1025 {
		t.Fatalf("power series = %d; want the 1025 distinct fresh points", strings.Count(body, "helianthus_native_power{"))
	}
	for _, absent := range []string{`point="stale_power",protocol="family.alpha",unit="watts"`, `point="text_point"`, `point="undocumented_unit"`} {
		if strings.Contains(body, absent) {
			t.Fatalf("%s was exported", absent)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `point="pack_temperature"`) && strings.Contains(line, "index=") {
			t.Fatalf("unindexed point carried an index: %s", line)
		}
	}
	// The retained values and units stay as the drivers published them.
	cut, err := runtimeTree.Capture(context.Background(), tree.CutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cut.Close() }()
	changes, _ := runtimeTree.Registry.Since(0)
	originals := map[string]string{"water_pressure": "1.25 unit.bar", "pack_temperature": "20 unit.celsius", "fan_speed": "3000 unit.revolution_per_minute"}
	checked := 0
	for _, change := range changes {
		if change.Kind != tree.ChangeMember {
			continue
		}
		descriptor, err := cut.Describe(context.Background(), change.Ref)
		if err != nil {
			t.Fatal(err)
		}
		want, ok := originals[descriptor.Local]
		if !ok {
			continue
		}
		value, err := cut.Read(context.Background(), change.Ref, tree.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(value.Encoded) + " " + string(descriptor.Unit); got != want {
			t.Fatalf("%s namespace value = %q; want %q", descriptor.Local, got, want)
		}
		checked++
	}
	if checked != 4 {
		t.Fatalf("checked %d namespace members; want 4", checked)
	}
	handler := newGatewayMetricsHandler(ebusgateway.NewBusObservabilityStore(ebusgateway.Config{}), metrics)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `helianthus_native_rotational_speed{device="node.battery",driver_instance="inst.c",point="fan_speed",protocol="family.beta",unit="hertz"} 50`) {
		t.Fatalf("route did not serve the namespace points: %d", response.Code)
	}
}

func TestNativePointNamespaceReadStates(t *testing.T) {
	const received = int64(92 * time.Second)
	retained, stale, zero := semreg.Uint64("90000000000"), semreg.Uint64("21500000000"), semreg.Uint64("0")
	published := []namespacePublished{
		{namespace: "family.alpha", instance: "inst.a", kind: "node.outdoor", point: "fresh_pressure", unit: "unit.bar", channel: "4", encoded: "1.25", freshness: semreg.FreshnessFresh, received: received, age: &retained},
		{namespace: "family.beta", instance: "inst.b", kind: "node.meter", point: "stale_power", unit: "unit.watt", encoded: "12.5", freshness: semreg.FreshnessStale, received: received, age: &stale},
		{namespace: "family.beta", instance: "inst.b", kind: "node.meter", point: "absent_power", unit: "unit.watt", absent: true, received: received, age: &zero},
		{namespace: "family.beta", instance: "inst.b", kind: "node.meter", point: "failed_power", unit: "unit.watt", readErr: errors.New("retained read failed")},
		{namespace: "family.beta", instance: "inst.c", kind: "node.meter", point: "failed_power", unit: "unit.watt", encoded: "7", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.beta", instance: "inst.c", kind: "node.meter", point: "absent_power", unit: "unit.watt", encoded: "8", freshness: semreg.FreshnessFresh, received: received},
		{namespace: "family.beta", instance: "inst.b", kind: "node.meter", point: "derived_power", unit: "unit.watt", encoded: "13", freshness: semreg.FreshnessFresh, received: received, derived: true},
	}
	metrics, err := newGatewaySemanticMetrics(nil, gatewaySemanticMetricRegistry())
	if err != nil {
		t.Fatal(err)
	}
	metrics.now = func() time.Time { return time.Unix(101, 0).UTC() }
	metrics.tree = namespaceFixture(t, published)
	handler := newGatewayMetricsHandler(ebusgateway.NewBusObservabilityStore(ebusgateway.Config{}), metrics)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", response.Code)
	}
	for _, tc := range []struct {
		point string
		want  []string
	}{
		{"fresh_pressure", []string{
			`helianthus_native_point_age{device="node.outdoor",driver_instance="inst.a",index="4",point="fresh_pressure",protocol="family.alpha",unit="seconds"} 90`,
			`helianthus_native_pressure{device="node.outdoor",driver_instance="inst.a",index="4",point="fresh_pressure",protocol="family.alpha",unit="pascals"} 125000`,
		}},
		{"stale_power", []string{
			`helianthus_native_point_age{device="node.meter",driver_instance="inst.b",point="stale_power",protocol="family.beta",unit="seconds"} 21.5`,
		}},
		{"absent_power", []string{
			`helianthus_native_point_age{device="node.meter",driver_instance="inst.b",point="absent_power",protocol="family.beta",unit="seconds"} 0`,
			`helianthus_native_point_age{device="node.meter",driver_instance="inst.c",point="absent_power",protocol="family.beta",unit="seconds"} 9`,
			`helianthus_native_power{device="node.meter",driver_instance="inst.c",point="absent_power",protocol="family.beta",unit="watts"} 8`,
		}},
		{"failed_power", []string{
			`helianthus_native_point_age{device="node.meter",driver_instance="inst.c",point="failed_power",protocol="family.beta",unit="seconds"} 9`,
			`helianthus_native_power{device="node.meter",driver_instance="inst.c",point="failed_power",protocol="family.beta",unit="watts"} 7`,
		}},
		{"derived_power", nil},
	} {
		t.Run(tc.point, func(t *testing.T) {
			var got []string
			for _, line := range strings.Split(response.Body.String(), "\n") {
				if strings.Contains(line, `point="`+tc.point+`"`) {
					got = append(got, line)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Errorf("samples = %q; want %q", got, tc.want)
			}
		})
	}

	// The tree reports a failed retained callback as unavailable and holds no
	// age for it. It is still an observation, even though neither sample exists.
	points, err := metrics.namespaceObservations(context.Background(), metrics.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		if point.Point == "failed_power" && point.DriverInstance == "inst.b" {
			if point.Fresh || point.NativeValue != nil || point.AgeSeconds != nil {
				t.Fatalf("failed point = %+v; want unknown with no held age", point)
			}
			return
		}
	}
	t.Fatal("unavailable point was dropped from the namespace observations")
}

func TestNativePointFailedReadWithHeldAge(t *testing.T) {
	// A failed callback on the pinned tree drops its metadata. Exercise the
	// value envelope separately so a failure that supplies an age keeps it.
	point := ebusgateway.NativeObservation{
		Protocol: "family.alpha", Device: "node.meter", DriverInstance: "inst.a",
		Point: "power", Quantity: "power", NativeUnit: "watt",
	}
	age := semreg.Uint64("3000000000")
	for _, tc := range []struct {
		name    string
		status  tree.Status
		readErr error
	}{
		{"read_error", tree.StatusLiveOnly, errors.New("retained read failed")},
		{"unavailable", tree.StatusUnavailable, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation, ok := namespaceReadObservation(point, tree.Value{
				Status: tc.status, Age: &age, Encoded: []byte("5"),
			}, tc.readErr, time.Unix(101, 0))
			if !ok {
				t.Fatal("failed read was dropped despite its held age")
			}
			text := ebusgateway.RenderNativePrometheus([]ebusgateway.NativeObservation{observation})
			const ageSample = `helianthus_native_point_age{device="node.meter",driver_instance="inst.a",point="power",protocol="family.alpha",unit="seconds"} 3`
			if !strings.Contains(text, ageSample+"\n") || strings.Contains(text, "helianthus_native_power{") {
				t.Fatalf("wanted age %q and no value, got:\n%s", ageSample, text)
			}
		})
	}
}
