package semanticmetrics

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Project-Helianthus/helianthus-gateway/internal/semanticview"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

func TestSlotLifecyclesCompareExactly(t *testing.T) {
	persistence := &o21Memory{}
	store, err := O21Create(persistence, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, registry := m36Fixture(t, "infrastructure.ac.voltage", m36Quantity("230", "unit.volt"))
	base := registry.Bindings[0]
	first := cutMember(base, "resource:zone.a", true)
	second := cutMember(base, "resource:zone.b", true)
	if _, err = store.Sync("lifecycle", []CutMember{second, first}); err != nil {
		t.Fatal(err)
	}
	if err = store.Revalidate("lifecycle", store.Export().Bindings); err != nil {
		t.Fatal(err)
	}
	view := lifecycleView(t, snapshot, "resource:zone.a")
	leftSeries := lifecycleSeries(t, view, store.Export())
	leftSlots := lifecycleSlots(store.Export().Bindings)
	restarted, err := O21Open(persistence, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	superseded := first
	superseded.SourceEpochID = "epoch:next"
	if _, err = restarted.Sync("lifecycle", []CutMember{superseded, second}); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Revalidate("lifecycle", restarted.Export().Bindings); err != nil {
		t.Fatal(err)
	}
	rightSeries := lifecycleSeries(t, view, restarted.Export())
	rightSlots := lifecycleSlots(restarted.Export().Bindings)
	if leftSeries != rightSeries || leftSlots != rightSlots {
		t.Fatalf("lifecycles diverged\nseries\n%s\n%s\nslots\n%s\n%s", leftSeries, rightSeries, leftSlots, rightSlots)
	}
	if !strings.Contains(leftSeries, "helianthus_infrastructure_ac_voltage_volts{slot=\"0\"} 230") {
		t.Fatal(leftSeries)
	}
	replacement := cutMember(base, "resource:zone.c", true)
	withdrawn := first
	withdrawn.Current = false
	if _, err = restarted.Sync("lifecycle", []CutMember{withdrawn, second, replacement}); err != nil {
		t.Fatal(err)
	}
	slots := map[string]int{}
	retired := map[string]bool{}
	for _, binding := range restarted.Export().Bindings {
		slots[string(binding.ResourceID)] = binding.Slot
		retired[string(binding.ResourceID)] = binding.Retired
	}
	if !retired["resource:zone.a"] || retired["resource:zone.b"] || retired["resource:zone.c"] {
		t.Fatalf("retirement = %#v", retired)
	}
	if slots["resource:zone.c"] == slots["resource:zone.a"] {
		t.Fatal("retired slot was reused")
	}
}

func TestThirtyThirdAssignmentExports(t *testing.T) {
	persistence := &o21Memory{}
	store, err := O21Create(persistence, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, registry := m36Fixture(t, "infrastructure.ac.voltage", m36Quantity("230", "unit.volt"))
	base := registry.Bindings[0]
	var members []CutMember
	for i := 0; i < 33; i++ {
		members = append(members, cutMember(base, semreg.ResourceID("resource:slot-"+itoa(i)), true))
	}
	if _, err = store.Sync("lifecycle", members); err != nil {
		t.Fatal(err)
	}
	restarted, err := O21Open(persistence, "lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Export().Bindings) != 33 {
		t.Fatalf("stored %d", len(restarted.Export().Bindings))
	}
	if err = restarted.Revalidate("lifecycle", restarted.Export().Bindings); err != nil {
		t.Fatal(err)
	}
	registry = restarted.Export()
	var slot int
	for _, binding := range registry.Bindings {
		if binding.ResourceID == "resource:slot-32" {
			slot = binding.Slot
		}
	}
	view := lifecycleView(t, snapshot, "resource:slot-32")
	text := m36Render(t, view, registry)
	if !strings.Contains(text, "helianthus_infrastructure_ac_voltage_volts{slot=\""+itoa(slot)+"\"} 230") {
		t.Fatal(text)
	}
}

func TestO21OldKeyIsNotRead(t *testing.T) {
	store, persistence, binding := o21Fixture(t)
	o21Assign(t, store, binding)
	var document map[string]any
	if err := json.Unmarshal(persistence.raw, &document); err != nil {
		t.Fatal(err)
	}
	rows := document["Bindings"].([]any)
	delete(rows[0].(map[string]any), "ResourceID")
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	persistence.raw = raw
	if _, err = O21Open(persistence, "o21-target"); err == nil {
		t.Fatal("old key was read")
	}
}

func cutMember(base Binding, id semreg.ResourceID, current bool) CutMember {
	text := string(id)
	dimensions := append([]semreg.Dimension(nil), base.Dimensions...)
	dimensions[0].Value.Text = &text
	return CutMember{
		ResourceID: id, FactID: base.Fields[0], Dimensions: dimensions, Domain: base.Domain,
		AssetID: base.AssetID, SourceID: base.SourceID, SourceEpochID: base.SourceEpochID,
		BindingID: base.BindingID, DriverGeneration: base.DriverGeneration, ServiceInstance: base.ServiceInstance,
		Current: current,
	}
}

func lifecycleView(t *testing.T, snapshot semreg.Snapshot, id string) semanticview.View {
	t.Helper()
	text := id
	snapshot.Facts[0].Key.Dimensions[0].Value.Text = &text
	snapshot.Facts[0].Candidates[0].Key.Dimensions[0].Value.Text = &text
	return m36View(t, snapshot)
}

func lifecycleSeries(t *testing.T, view semanticview.View, registry Registry) string {
	t.Helper()
	return m36Render(t, view, registry)
}

func lifecycleSlots(bindings []Binding) string {
	var rows []string
	for _, binding := range bindings {
		state := "active"
		if binding.Retired {
			state = "retired"
		}
		rows = append(rows, string(binding.ResourceID)+" "+string(binding.Fields[0])+" "+itoa(binding.Slot)+" "+state)
	}
	return strings.Join(rows, "\n")
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
