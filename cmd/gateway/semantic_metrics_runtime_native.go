package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	ebusgateway "github.com/Project-Helianthus/helianthus-gateway"
	"github.com/Project-Helianthus/helianthus-semreg/semreg/runtime/tree"
	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

// namespaceUnit is the quantity and the unit name a published unit identifier
// stands for in the native renderer. The renderer converts the value into the
// unit its family fixes; the published value and unit stay as they are in the
// namespace.
type namespaceUnit struct {
	quantity string
	unit     string
}

var namespaceUnits = map[semreg.DefinitionID]namespaceUnit{
	"unit.kelvin":                 {"temperature", "kelvin"},
	"unit.celsius":                {"temperature", "celsius"},
	"unit.pascal":                 {"pressure", "pascal"},
	"unit.bar":                    {"pressure", "bar"},
	"unit.cubic_metre_per_second": {"volume_flow", "cubic_meter_per_second"},
	"unit.litre_per_minute":       {"volume_flow", "litre_per_minute"},
	"unit.joule":                  {"energy", "joule"},
	"unit.kilowatt_hour":          {"energy", "kilowatt_hour"},
	"unit.watt":                   {"power", "watt"},
	"unit.second":                 {"time", "second"},
	"unit.ratio":                  {"ratio", "ratio"},
	"unit.percent":                {"ratio", "percent"},
	"unit.count":                  {"count", "count"},
	"unit.revolution_per_minute":  {"rotational_speed", "revolution_per_minute"},
	"unit.hertz":                  {"frequency", "hertz"},
	"unit.ampere":                 {"current", "ampere"},
	"unit.volt":                   {"voltage", "volt"},
}

// namespaceObservations reads every numeric attribute member the drivers have
// registered in their namespaces, from the retained observations of one tree
// cut. Nothing is sent to a device. The protocol label is the namespace the
// driver published, the device label is the kind of the node that carries the
// member, the point label is the member's stable local identifier, the index
// label is the member's channel when it publishes one, and driver_instance is
// the instance of the member key. A member whose unit is not a published
// numeric unit, or whose retained value is not a number, is absent.
func (m *gatewaySemanticMetrics) namespaceObservations(ctx context.Context, at time.Time) ([]ebusgateway.NativeObservation, error) {
	if m.tree == nil {
		return nil, nil
	}
	changes, ok := m.tree.Registry.Since(0)
	if !ok {
		return nil, errors.New("namespace registry history unavailable")
	}
	cut, err := m.tree.Capture(ctx, tree.CutOptions{})
	if err != nil {
		return nil, fmt.Errorf("namespace capture: %w", err)
	}
	defer func() { _ = cut.Close() }()
	seen := map[tree.Ref]bool{}
	var out []ebusgateway.NativeObservation
	for _, change := range changes {
		if change.Kind != tree.ChangeMember || seen[change.Ref] {
			continue
		}
		seen[change.Ref] = true
		if point, ok := namespacePoint(ctx, cut, change.Ref, at); ok {
			out = append(out, point)
		}
	}
	return out, nil
}

func namespacePoint(ctx context.Context, cut *tree.TreeCut, ref tree.Ref, at time.Time) (ebusgateway.NativeObservation, bool) {
	descriptor, err := cut.Describe(ctx, ref)
	if err != nil || descriptor.Withdrawn {
		return ebusgateway.NativeObservation{}, false
	}
	unit, ok := namespaceUnits[descriptor.Unit]
	if !ok {
		return ebusgateway.NativeObservation{}, false
	}
	key, err := tree.ParseKey(string(ref))
	if err != nil {
		return ebusgateway.NativeObservation{}, false
	}
	node, err := cut.Describe(ctx, tree.Ref(key.Node.String()))
	if err != nil || node.NodeKind == "" {
		return ebusgateway.NativeObservation{}, false
	}
	value, err := cut.Read(ctx, ref, tree.ReadOptions{})
	if err != nil {
		return ebusgateway.NativeObservation{}, false
	}
	point := ebusgateway.NativeObservation{
		Protocol: string(descriptor.Namespace), Device: string(node.NodeKind), Point: descriptor.Local,
		DriverInstance: string(descriptor.Instance.Instance), Quantity: unit.quantity, NativeUnit: unit.unit,
	}
	if descriptor.Channel != "" {
		point.Index, point.Indexed = descriptor.Channel, true
	}
	if value.ReceivedAt != nil {
		if nanoseconds, err := strconv.ParseInt(string(value.ReceivedAt.UnixNanoseconds), 10, 64); err == nil {
			age := at.Sub(time.Unix(0, nanoseconds)).Seconds()
			point.AgeSeconds = &age
		}
	}
	switch value.Status {
	case tree.StatusCurrent, tree.StatusLiveOnly, tree.StatusChanged:
		number, err := strconv.ParseFloat(strings.TrimSpace(string(value.Encoded)), 64)
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return ebusgateway.NativeObservation{}, false
		}
		point.NativeValue, point.Fresh = &number, true
	case tree.StatusStale:
	default:
		return ebusgateway.NativeObservation{}, false
	}
	return point, true
}
