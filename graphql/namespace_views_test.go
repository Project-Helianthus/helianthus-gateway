package graphql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/driver"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/v1/graph"
	gql "github.com/graphql-go/graphql"
)

type viewFixture struct {
	runtime *tree.Tree
	ctx     context.Context
	reads   int
}

type viewBinding struct {
	namespace driver.NamespaceID
	instance  driver.InstanceKey
	reg       *tree.InstanceRegistry
}

func (b viewBinding) member(local string) driver.MemberKey {
	return driver.MemberKey{Namespace: b.namespace, Instance: b.instance, Node: driver.NodeID(b.reg.Root().ID), Local: driver.MemberID(local)}
}

// publish registers a readable member on the instance root and returns the
// key that binds a view field to it.
func (f *viewFixture) publish(t *testing.T, b viewBinding, local, encoded string) driver.MemberKey {
	t.Helper()
	namespaceMust(t, b.reg.ReadMember(b.reg.Root(), driver.MemberSpec{Local: driver.MemberID(local), Schema: "schema.view", Units: "unit.view", Mode: "retained", Scope: "diagnostics"}, func(context.Context) ([]byte, error) {
		f.reads++
		return []byte(encoded), nil
	}))
	return b.member(local)
}

func newViewFixture(t *testing.T, namespaces map[driver.NamespaceID][]driver.InstanceKey, declare func(f *viewFixture, bind map[string]viewBinding)) *viewFixture {
	t.Helper()
	catalog, err := graph.Embedded()
	namespaceMust(t, err)
	cut, err := graph.AssembleCut(nil, catalog, graph.CutComplete)
	namespaceMust(t, err)
	store := graph.NewCutStore(nil)
	namespaceMust(t, store.Retain(cut, nil, catalog))
	runtime, err := tree.New(tree.Config{
		Registry:   tree.RegistryConfig{Installation: "synthetic", Bridge: "bridge.synthetic", Incarnation: "incarnation.synthetic", Lifecycle: namespaceLifecycle{}},
		Cuts:       namespaceCutSource{view: tree.CutView{Ref: graph.CutRef{CutID: cut.ID(), Completeness: cut.Completeness(), Members: cut.Members()}, Cut: cut, Store: store, Catalog: catalog}},
		Operations: struct{ tree.OperationPort }{},
	})
	namespaceMust(t, err)
	t.Cleanup(runtime.Registry.Close)
	f := &viewFixture{runtime: runtime, ctx: context.Background()}
	bind := map[string]viewBinding{}
	for namespace, instances := range namespaces {
		namespaceMust(t, runtime.Registry.Reserve(driver.Namespace{Owner: driver.DriverKind("owner_" + string(namespace)), Key: namespace}, instances))
		for _, instance := range instances {
			reg, err := runtime.Registry.Bind(namespace, instance, 1)
			namespaceMust(t, err)
			bind[string(namespace)+"/"+string(instance)] = viewBinding{namespace, instance, reg}
		}
	}
	declare(f, bind)
	return f
}

func (f *viewFixture) fields(t *testing.T) (gql.Fields, error) {
	t.Helper()
	cut, err := f.runtime.Capture(f.ctx, tree.CutOptions{})
	namespaceMust(t, err)
	t.Cleanup(func() { namespaceMust(t, cut.Close()) })
	query, _, err := NamespaceFields(f.ctx, f.runtime, cut, tree.NodeRef{ID: semreg.ResourceID(namespaceRootPath[1:]), Bridge: "bridge.synthetic"}, semanticOperationField(NewBuilder(nil, nil)))
	return query, err
}

func (f *viewFixture) schema(t *testing.T) gql.Schema {
	t.Helper()
	query, err := f.fields(t)
	namespaceMust(t, err)
	schema, err := gql.NewSchema(gql.SchemaConfig{Query: gql.NewObject(gql.ObjectConfig{Name: "query", Fields: query})})
	namespaceMust(t, err)
	return schema
}

func viewScalar(name string, nullable bool) driver.ViewType {
	return driver.ViewType{Name: name, Nullable: nullable}
}

func viewRecordOf(name string, list, nullable bool) driver.ViewType {
	return driver.ViewType{Name: name, Record: true, List: list, Nullable: nullable}
}

func viewFixtureOnlyNamespace(namespace driver.NamespaceID, instances ...driver.InstanceKey) map[driver.NamespaceID][]driver.InstanceKey {
	return map[driver.NamespaceID][]driver.InstanceKey{namespace: instances}
}

func viewJSON(t *testing.T, result *gql.Result) string {
	t.Helper()
	if len(result.Errors) != 0 {
		t.Fatalf("unexpected GraphQL errors: %+v", result.Errors)
	}
	out, err := json.Marshal(result.Data)
	namespaceMust(t, err)
	return string(out)
}

// A view that declares a small part of the existing dhw type answers exactly
// as the existing resolver does for the same values, and carries the same
// field names, types and unit.
func TestNamespaceViewMatchesExistingTypedResolver(t *testing.T) {
	temperature, setpoint := 48.5, 52.0
	for _, tc := range []struct {
		name   string
		status *DhwStatus
		state  string
		config string
	}{
		{"values", &DhwStatus{State: DhwState{CurrentTempC: &temperature}, Config: DhwConfig{OperatingMode: "auto", TargetTempC: &setpoint}},
			`{"dhw_temperature":48.5}`, `{"dhw_operating_mode":"auto","dhw_temperature_setpoint":52}`},
		{"unset", &DhwStatus{}, `{"dhw_temperature":null}`, `{"dhw_operating_mode":null,"dhw_temperature_setpoint":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := NewBuilder(nil, nil)
			provider := NewLiveSemanticProvider()
			provider.SetDHW(tc.status)
			builder.SetSemanticProvider(provider)
			old, err := NewQuerySchema(builder)
			namespaceMust(t, err)
			want := gql.Do(gql.Params{Schema: old, RequestString: `{ dhw { state { dhw_temperature } config { dhw_operating_mode dhw_temperature_setpoint } } }`})

			f := newViewFixture(t, viewFixtureOnlyNamespace("sample", "one"), func(f *viewFixture, bind map[string]viewBinding) {
				b := bind["sample/one"]
				state, config := f.publish(t, b, "state", tc.state), f.publish(t, b, "config", tc.config)
				namespaceMust(t, b.reg.View(driver.ViewSpec{Root: "dhw_status", Records: []driver.ViewRecord{
					{Name: "dhw_status", Fields: []driver.ViewField{
						{Name: "state", Type: viewRecordOf("dhw_state", false, false), Member: state},
						{Name: "config", Type: viewRecordOf("dhw_config", false, false), Member: config},
					}},
					{Name: "dhw_state", Fields: []driver.ViewField{{Name: "dhw_temperature", Type: viewScalar("Float", true), Unit: "celsius", Member: state}}},
					{Name: "dhw_config", Fields: []driver.ViewField{
						{Name: "dhw_operating_mode", Type: viewScalar("String", true), Member: config},
						{Name: "dhw_temperature_setpoint", Type: viewScalar("Float", true), Unit: "celsius", Member: config},
					}},
				}}))
			})
			schema := f.schema(t)
			got := namespaceExecute(f.ctx, schema, `{ sample_view_dhw_status(instance:"one") { state { dhw_temperature } config { dhw_operating_mode dhw_temperature_setpoint } } }`, nil)
			oldJSON := strings.Replace(viewJSON(t, want), `"dhw"`, `"sample_view_dhw_status"`, 1)
			if newJSON := viewJSON(t, got); newJSON != oldJSON {
				t.Fatalf("view answer %s differs from the existing resolver %s", newJSON, oldJSON)
			}
			for _, field := range []struct{ typ, name string }{{"dhw_state", "dhw_temperature"}, {"dhw_config", "dhw_operating_mode"}, {"dhw_config", "dhw_temperature_setpoint"}} {
				oldField := old.Type(field.typ).(*gql.Object).Fields()[field.name]
				newField := schema.Type(field.typ).(*gql.Object).Fields()[field.name]
				if oldField.Type.String() != newField.Type.String() {
					t.Fatalf("%s.%s: type %s, existing %s", field.typ, field.name, newField.Type, oldField.Type)
				}
			}
			if got := schema.Type("dhw_state").(*gql.Object).Fields()["dhw_temperature"].Description; got != "unit: celsius" {
				t.Fatalf("declared unit not served: %q", got)
			}
			if f.reads == 0 {
				t.Fatal("view did not read its bound members")
			}
		})
	}
}

// Documented field names and offsets of the public B555 forms fixture: the
// availability bits, the positive counts and the positive slot. Raw values are
// decoded here from the synthetic reply bytes by those offsets, not by the
// engine.
func TestNamespaceViewServesEveryDocumentedFieldWithNoPriorType(t *testing.T) {
	const grade = "Hypothesis"
	availability := []byte{0x15, 0x00, 0x21, 0x43, 0x65, 0x87, 0xA9, 0xCB}
	counts := []byte{0x00, 1, 2, 3, 4, 5, 6, 7, 8}
	slot := []byte{0x00, 7, 30, 17, 0, 0x02, 0x01}
	bits := []string{"dhw", "circulation", "noise_reduction", "ventilation", "high_tariff"}
	days := []string{"status", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "count_24_hour"}
	slotFields := []string{"status", "start_hour", "start_minute", "end_hour", "end_minute"}

	cell := func(raw int, interpretation string) string {
		return fmt.Sprintf(`{"raw":%d,"interpretation":%q,"grade":%q}`, raw, interpretation, grade)
	}
	object := func(names []string, value func(i int) string) string {
		var parts []string
		for i, n := range names {
			parts = append(parts, fmt.Sprintf("%q:%s", n, value(i)))
		}
		return strings.Join(parts, ",")
	}
	var channels []string
	for n := 1; n <= 12; n++ {
		value := int(availability[2+(n-1)/2] >> (4 * ((n - 1) % 2)) & 0xF)
		channels = append(channels, fmt.Sprintf(`{"channel":%d,"value":%d}`, n, value))
	}
	availabilityJSON := "{" + object(bits, func(i int) string { return cell(int(availability[0]>>i&1), "bit") }) + `,"channels":[` + strings.Join(channels, ",") + "]}"
	countsJSON := "{" + object(days, func(i int) string { return cell(int(counts[i]), "u8") }) + "}"
	// setpoint_raw is u16 little endian; its unit is documented as unknown, so none is declared.
	slotJSON := "{" + object(slotFields, func(i int) string { return cell(int(slot[i]), "u8") }) + `,"setpoint_raw":` + cell(int(slot[5])|int(slot[6])<<8, "u16_le") + "}"

	f := newViewFixture(t, viewFixtureOnlyNamespace("sample", "one"), func(f *viewFixture, bind map[string]viewBinding) {
		b := bind["sample/one"]
		forms := map[string]driver.MemberKey{
			"availability":    f.publish(t, b, "availability", availabilityJSON),
			"positive_counts": f.publish(t, b, "positive_counts", countsJSON),
			"positive_slot":   f.publish(t, b, "positive_slot", slotJSON),
		}
		cellField := func(form, name string) driver.ViewField {
			return driver.ViewField{Name: name, Type: viewRecordOf("b555_field", false, false), Member: forms[form]}
		}
		record := func(form string, names ...string) driver.ViewRecord {
			r := driver.ViewRecord{Name: "b555_" + form}
			for _, n := range names {
				r.Fields = append(r.Fields, cellField(form, n))
			}
			return r
		}
		availabilityRecord := record("availability", bits...)
		availabilityRecord.Fields = append(availabilityRecord.Fields, driver.ViewField{Name: "channels", Type: viewRecordOf("b555_channel", true, false), Member: forms["availability"]})
		root := driver.ViewRecord{Name: "b555_forms"}
		for _, form := range []string{"availability", "positive_counts", "positive_slot"} {
			root.Fields = append(root.Fields, driver.ViewField{Name: form, Type: viewRecordOf("b555_"+form, false, false), Member: forms[form]})
		}
		namespaceMust(t, b.reg.View(driver.ViewSpec{Root: "b555_forms", Records: []driver.ViewRecord{
			root, availabilityRecord, record("positive_counts", days...), record("positive_slot", append(slotFields, "setpoint_raw")...),
			{Name: "b555_field", Fields: []driver.ViewField{
				{Name: "raw", Type: viewScalar("Int", false), Member: forms["availability"]},
				{Name: "interpretation", Type: viewScalar("String", true), Member: forms["availability"]},
				{Name: "grade", Type: viewScalar("String", false), Member: forms["availability"]},
			}},
			{Name: "b555_channel", Fields: []driver.ViewField{
				{Name: "channel", Type: viewScalar("Int", false), Member: forms["availability"]},
				{Name: "value", Type: viewScalar("Int", false), Member: forms["availability"]},
			}},
		}}))
	})
	schema := f.schema(t)

	sub := func(names []string) string {
		var parts []string
		for _, n := range names {
			parts = append(parts, n+"{raw interpretation grade}")
		}
		return strings.Join(parts, " ")
	}
	query := fmt.Sprintf(`{ sample_view_b555_forms(instance:"one") {
 a:availability{ %s channels{channel value} }
 c:positive_counts{ %s }
 s:positive_slot{ %s setpoint_raw{raw interpretation grade} }
} }`, sub(bits), sub(days), sub(slotFields))
	var got map[string]map[string]any
	raw, err := json.Marshal(namespaceExecute(f.ctx, schema, query, nil).Data.(map[string]any)["sample_view_b555_forms"])
	namespaceMust(t, err)
	namespaceMust(t, json.Unmarshal(raw, &got))
	check := func(root string, names []string, wantRaw func(i int) int, wantInterp string) {
		for i, n := range names {
			cell, _ := got[root][n].(map[string]any)
			if cell == nil || cell["raw"] != float64(wantRaw(i)) || cell["interpretation"] != wantInterp || cell["grade"] != grade {
				t.Fatalf("%s.%s = %v; want raw %d, %s, %s", root, n, got[root][n], wantRaw(i), wantInterp, grade)
			}
		}
	}
	check("a", bits, func(i int) int { return int(availability[0] >> i & 1) }, "bit")
	check("c", days, func(i int) int { return int(counts[i]) }, "u8")
	check("s", slotFields, func(i int) int { return int(slot[i]) }, "u8")
	if cell := got["s"]["setpoint_raw"].(map[string]any); cell["raw"] != float64(0x0102) || cell["interpretation"] != "u16_le" {
		t.Fatalf("setpoint_raw = %v", cell)
	}
	served, _ := got["a"]["channels"].([]any)
	if len(served) != 12 {
		t.Fatalf("channels = %v", served)
	}
	for i, c := range served {
		cell := c.(map[string]any)
		want := int(availability[2+i/2] >> (4 * (i % 2)) & 0xF)
		if cell["channel"] != float64(i+1) || cell["value"] != float64(want) {
			t.Fatalf("channel %d = %v; want value %d", i+1, cell, want)
		}
	}
	if d := schema.Type("b555_positive_slot").(*gql.Object).Fields()["setpoint_raw"].Description; d != "" {
		t.Fatalf("unknown unit was declared: %q", d)
	}
}

// The key of a served view is its namespace, its instance and its root record.
// Each part is varied below; a resolver that ignored one of them would answer
// with another part's value.
func TestNamespaceViewKeyVariesNamespaceInstanceAndRoot(t *testing.T) {
	declare := func(f *viewFixture, b viewBinding, root, value string) {
		member := f.publish(t, b, "amount", value)
		namespaceMust(t, b.reg.View(driver.ViewSpec{Root: root, Records: []driver.ViewRecord{{Name: root, Fields: []driver.ViewField{
			{Name: "amount", Type: viewScalar("Int", false), Unit: "count", Member: member},
		}}}}))
	}
	f := newViewFixture(t, map[driver.NamespaceID][]driver.InstanceKey{"sample": {"one", "two"}, "aux": {"one"}}, func(f *viewFixture, bind map[string]viewBinding) {
		declare(f, bind["sample/one"], "reading", "1")
		declare(f, bind["sample/two"], "reading", "2")
		declare(f, bind["aux/one"], "reading", "3")
		extra := f.publish(t, bind["sample/one"], "other", "4")
		namespaceMust(t, bind["sample/one"].reg.View(driver.ViewSpec{Root: "second", Records: []driver.ViewRecord{{Name: "second", Fields: []driver.ViewField{
			{Name: "amount", Type: viewScalar("Int", false), Member: extra},
		}}}}))
	})
	schema := f.schema(t)
	for _, tc := range []struct{ field, instance, want string }{
		{"sample_view_reading", "one", "1"},
		{"sample_view_reading", "two", "2"},
		{"aux_view_reading", "one", "3"},
		{"sample_view_second", "one", "4"},
	} {
		got := viewJSON(t, namespaceExecute(f.ctx, schema, fmt.Sprintf(`{ %s(instance:%q){ amount } }`, tc.field, tc.instance), nil))
		if want := fmt.Sprintf(`{%q:{"amount":%s}}`, tc.field, tc.want); got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	namespaceAssertCode(t, namespaceExecute(f.ctx, schema, `{ aux_view_reading(instance:"two"){ amount } }`, nil), "aux_view_reading", "dangling_reference")
}

func TestNamespaceViewRejectsWhatItCannotServe(t *testing.T) {
	one := func(f *viewFixture, b viewBinding, name string, typ driver.ViewType, encoded string) {
		member := f.publish(t, b, "amount", encoded)
		namespaceMust(t, b.reg.View(driver.ViewSpec{Root: name, Records: []driver.ViewRecord{{Name: name, Fields: []driver.ViewField{{Name: "amount", Type: typ, Member: member}}}}}))
	}
	t.Run("a value that is not the declared type", func(t *testing.T) {
		f := newViewFixture(t, viewFixtureOnlyNamespace("sample", "one"), func(f *viewFixture, bind map[string]viewBinding) {
			one(f, bind["sample/one"], "reading", viewScalar("Int", false), `"text"`)
		})
		namespaceAssertCode(t, namespaceExecute(f.ctx, f.schema(t), `{ sample_view_reading(instance:"one"){ amount } }`, nil), "sample_view_reading", "invalid_value")
	})
	t.Run("a scalar the engine does not serve", func(t *testing.T) {
		f := newViewFixture(t, viewFixtureOnlyNamespace("sample", "one"), func(f *viewFixture, bind map[string]viewBinding) {
			one(f, bind["sample/one"], "reading", viewScalar("Decimal", false), `1`)
		})
		query, err := f.fields(t)
		namespaceMust(t, err)
		namespaceAssertCode(t, namespaceExecute(f.ctx, viewSchemaOf(t, query), `{ sample_view_reading }`, nil), "sample_view_reading", "invalid_value")
	})
	t.Run("one record name with two shapes", func(t *testing.T) {
		f := newViewFixture(t, map[driver.NamespaceID][]driver.InstanceKey{"sample": {"one"}, "aux": {"one"}}, func(f *viewFixture, bind map[string]viewBinding) {
			one(f, bind["sample/one"], "reading", viewScalar("Int", false), `1`)
			one(f, bind["aux/one"], "reading", viewScalar("String", false), `"1"`)
		})
		query, err := f.fields(t)
		namespaceMust(t, err)
		schema := viewSchemaOf(t, query)
		if got := viewJSON(t, namespaceExecute(f.ctx, schema, `{ aux_view_reading(instance:"one"){ amount } }`, nil)); got != `{"aux_view_reading":{"amount":"1"}}` {
			t.Fatalf("first declaration lost: %s", got)
		}
		namespaceAssertCode(t, namespaceExecute(f.ctx, schema, `{ sample_view_reading }`, nil), "sample_view_reading", "invalid_value")
	})
}

func viewSchemaOf(t *testing.T, query gql.Fields) gql.Schema {
	t.Helper()
	schema, err := gql.NewSchema(gql.SchemaConfig{Query: gql.NewObject(gql.ObjectConfig{Name: "query", Fields: query})})
	namespaceMust(t, err)
	return schema
}

// A record that names itself is served, and does not recurse without end.
func TestNamespaceViewServesRecursiveRecord(t *testing.T) {
	f := newViewFixture(t, viewFixtureOnlyNamespace("sample", "one"), func(f *viewFixture, bind map[string]viewBinding) {
		b := bind["sample/one"]
		member := f.publish(t, b, "node", `{"value":2,"child":null}`)
		value := f.publish(t, b, "value", `1`)
		namespaceMust(t, b.reg.View(driver.ViewSpec{Root: "node", Records: []driver.ViewRecord{{Name: "node", Fields: []driver.ViewField{
			{Name: "value", Type: viewScalar("Int", false), Member: value},
			{Name: "child", Type: viewRecordOf("node", false, true), Member: member},
		}}}}))
	})
	got := viewJSON(t, namespaceExecute(f.ctx, f.schema(t), `{ sample_view_node(instance:"one"){ value child { value child { value } } } }`, nil))
	if want := `{"sample_view_node":{"child":{"child":null,"value":2},"value":1}}`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// A declaration SemReg accepts never removes the other fields of the engine.
func TestNamespaceViewConflictLeavesOtherFieldsServed(t *testing.T) {
	f := newViewFixture(t, viewFixtureOnlyNamespace("sample", "one", "two"), func(f *viewFixture, bind map[string]viewBinding) {
		for instance, extra := range map[string]bool{"sample/one": false, "sample/two": true} {
			b := bind[instance]
			member := f.publish(t, b, "amount", `1`)
			fields := []driver.ViewField{{Name: "amount", Type: viewScalar("Int", false), Member: member}}
			if extra {
				fields = append(fields, driver.ViewField{Name: "more", Type: viewScalar("Int", true), Member: member})
			}
			namespaceMust(t, b.reg.View(driver.ViewSpec{Root: "reading", Records: []driver.ViewRecord{{Name: "reading", Fields: fields}}}))
		}
	})
	query, err := f.fields(t)
	namespaceMust(t, err)
	if query["tree_list"] == nil || query["sample_describe"] == nil && query["sample_read"] == nil && len(query) < 3 {
		t.Fatalf("other fields were lost: %d fields", len(query))
	}
	namespaceAssertCode(t, namespaceExecute(f.ctx, viewSchemaOf(t, query), `{ sample_view_reading }`, nil), "sample_view_reading", "invalid_value")
}

// Two distinct declarations never share a field name silently.
func TestNamespaceViewNameCollisionIsRefused(t *testing.T) {
	f := newViewFixture(t, map[driver.NamespaceID][]driver.InstanceKey{"a": {"one"}, "a_view_b": {"one"}}, func(f *viewFixture, bind map[string]viewBinding) {
		for key, root := range map[string]string{"a/one": "b_view_c", "a_view_b/one": "c"} {
			b := bind[key]
			member := f.publish(t, b, "amount", `1`)
			namespaceMust(t, b.reg.View(driver.ViewSpec{Root: root, Records: []driver.ViewRecord{{Name: root, Fields: []driver.ViewField{{Name: "amount", Type: viewScalar("Int", false), Member: member}}}}}))
		}
	})
	query, err := f.fields(t)
	namespaceMust(t, err)
	namespaceAssertCode(t, namespaceExecute(f.ctx, viewSchemaOf(t, query), `{ a_view_b_view_c }`, nil), "a_view_b_view_c", "invalid_value")
}
