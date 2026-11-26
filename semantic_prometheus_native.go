package ebusgateway

import (
	"bytes"
)

// NativeObservation is one numeric member a driver published in its namespace.
// Index is set only when Indexed is true, and then it is the documented
// instance index. DriverInstance is the namespace instance, not a point index.
// NativeValue and NativeUnit stay in the published unit; the renderer does
// not replace them.
type NativeObservation struct {
	Protocol       string
	Device         string
	Point          string
	DriverInstance string
	Index          string
	Indexed        bool
	Quantity       string
	NativeUnit     string
	NativeValue    *float64
	AgeSeconds     *float64
	Fresh          bool
}

type nativeFamily struct {
	quantity string
	name     string
	unit     string
	help     string
}

// nativeFamilies is the closed set of numeric native series. The unit label is
// the family's constant SI unit. Quantity names are not protocol names.
var nativeFamilies = []nativeFamily{
	{"temperature", "helianthus_native_temperature", "kelvin", "Native temperature in kelvin."},
	{"temperature_difference", "helianthus_native_temperature_difference", "kelvin", "Native temperature difference in kelvin."},
	{"pressure", "helianthus_native_pressure", "pascals", "Native pressure in pascals."},
	{"volume_flow", "helianthus_native_volume_flow", "cubic_meters_per_second", "Native volume flow in cubic meters per second."},
	{"energy", "helianthus_native_energy", "joules", "Native energy in joules."},
	{"power", "helianthus_native_power", "watts", "Native power in watts."},
	{"time", "helianthus_native_time", "seconds", "Native time in seconds."},
	{"ratio", "helianthus_native_ratio", "ratio", "Native ratio from 0 to 1."},
	{"count", "helianthus_native_count", "count", "Native count."},
	{"rotational_speed", "helianthus_native_rotational_speed", "hertz", "Native rotational speed in hertz."},
	{"frequency", "helianthus_native_frequency", "hertz", "Native frequency in hertz."},
	{"current", "helianthus_native_current", "amperes", "Native current in amperes."},
	{"voltage", "helianthus_native_voltage", "volts", "Native voltage in volts."},
}

// RenderNativePrometheus writes every published numeric native point and its
// held age. Unknown and stale points emit no value, and emit no samples when
// no age is held. An undecodable or undocumented point is absent. There is no
// count limit. Overflow is the number of value
// series omitted because their label set was already emitted, and it is 0
// when none were omitted.
func RenderNativePrometheus(points []NativeObservation) string {
	var buffer bytes.Buffer
	writeNativePoints(newPrometheusWriter(&buffer), points)
	return buffer.String()
}

func writeNativePoints(w *prometheusWriter, points []NativeObservation) {
	w.writeHelp("helianthus_native_temperature", "Native temperature in kelvin.")
	w.writeType("helianthus_native_temperature", "gauge")
	w.writeHelp("helianthus_native_temperature_difference", "Native temperature difference in kelvin.")
	w.writeType("helianthus_native_temperature_difference", "gauge")
	w.writeHelp("helianthus_native_pressure", "Native pressure in pascals.")
	w.writeType("helianthus_native_pressure", "gauge")
	w.writeHelp("helianthus_native_volume_flow", "Native volume flow in cubic meters per second.")
	w.writeType("helianthus_native_volume_flow", "gauge")
	w.writeHelp("helianthus_native_energy", "Native energy in joules.")
	w.writeType("helianthus_native_energy", "gauge")
	w.writeHelp("helianthus_native_power", "Native power in watts.")
	w.writeType("helianthus_native_power", "gauge")
	w.writeHelp("helianthus_native_time", "Native time in seconds.")
	w.writeType("helianthus_native_time", "gauge")
	w.writeHelp("helianthus_native_ratio", "Native ratio from 0 to 1.")
	w.writeType("helianthus_native_ratio", "gauge")
	w.writeHelp("helianthus_native_count", "Native count.")
	w.writeType("helianthus_native_count", "gauge")
	w.writeHelp("helianthus_native_rotational_speed", "Native rotational speed in hertz.")
	w.writeType("helianthus_native_rotational_speed", "gauge")
	w.writeHelp("helianthus_native_frequency", "Native frequency in hertz.")
	w.writeType("helianthus_native_frequency", "gauge")
	w.writeHelp("helianthus_native_current", "Native current in amperes.")
	w.writeType("helianthus_native_current", "gauge")
	w.writeHelp("helianthus_native_voltage", "Native voltage in volts.")
	w.writeType("helianthus_native_voltage", "gauge")
	w.writeHelp("helianthus_native_point_age", "Age of a held native observation, in seconds.")
	w.writeType("helianthus_native_point_age", "gauge")
	w.writeHelp("helianthus_native_render_overflow", "Native value series omitted from this scrape.")
	w.writeType("helianthus_native_render_overflow", "gauge")
	seen := map[string]struct{}{}
	omitted := 0
	for i := range points {
		point := points[i]
		labels, value, ok := nativeSample(point)
		if labels == nil {
			continue
		}
		age, held := nativeAge(point)
		if held {
			w.writeGaugeSample("helianthus_native_point_age", age, nativePointLabels(ageLabels(labels)))
		}
		if ok {
			if _, dup := seen[nativeSeriesKey(labels)]; dup {
				omitted++
				continue
			}
			seen[nativeSeriesKey(labels)] = struct{}{}
			writeNativeFamilySample(w, point.Quantity, value, labels)
		}
	}
	w.writeGaugeSample("helianthus_native_render_overflow", float64(omitted), nil)
}

func nativeAge(point NativeObservation) (float64, bool) {
	if point.AgeSeconds == nil || *point.AgeSeconds < 0 {
		return 0, false
	}
	return *point.AgeSeconds, true
}

func ageLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range labels {
		out[key] = value
	}
	out["unit"] = "seconds"
	return out
}

func nativeSeriesKey(labels map[string]string) string {
	return labels["protocol"] + "\x00" + labels["device"] + "\x00" + labels["index"] + "\x00" + labels["point"] + "\x00" + labels["unit"] + "\x00" + labels["driver_instance"]
}

func nativeSample(point NativeObservation) (map[string]string, float64, bool) {
	if point.Protocol == "" || point.Device == "" || point.Point == "" || point.DriverInstance == "" {
		return nil, 0, false
	}
	if point.Indexed && point.Index == "" {
		return nil, 0, false
	}
	unit, ok := nativeUnit(point.Quantity)
	if !ok {
		return nil, 0, false
	}
	labels := map[string]string{
		"protocol": point.Protocol, "device": point.Device, "point": point.Point,
		"unit": unit, "driver_instance": point.DriverInstance,
	}
	if point.Indexed {
		labels["index"] = point.Index
	}
	if point.NativeValue != nil {
		if _, converted := convertNative(point.Quantity, point.NativeUnit, *point.NativeValue); !converted {
			return nil, 0, false
		}
	}
	if !point.Fresh || point.NativeValue == nil {
		return labels, 0, false
	}
	value, _ := convertNative(point.Quantity, point.NativeUnit, *point.NativeValue)
	return labels, value, true
}

func nativeUnit(quantity string) (string, bool) {
	for _, family := range nativeFamilies {
		if family.quantity == quantity {
			return family.unit, true
		}
	}
	return "", false
}

func convertNative(quantity, unit string, value float64) (float64, bool) {
	switch quantity {
	case "temperature":
		switch unit {
		case "celsius":
			return value + 273.15, true
		case "kelvin":
			return value, true
		}
	case "temperature_difference":
		switch unit {
		case "celsius", "kelvin":
			return value, true
		}
	case "pressure":
		switch unit {
		case "bar":
			return value * 100000, true
		case "pascal":
			return value, true
		}
	case "volume_flow":
		switch unit {
		case "litre_per_minute":
			return value / 60000, true
		case "cubic_meter_per_second":
			return value, true
		}
	case "energy":
		switch unit {
		case "kilowatt_hour":
			return value * 3600000, true
		case "joule":
			return value, true
		}
	case "power":
		if unit == "watt" {
			return value, true
		}
	case "time":
		if unit == "second" {
			return value, true
		}
	case "ratio":
		switch unit {
		case "percent":
			return value / 100, true
		case "ratio":
			return value, true
		}
	case "count":
		if unit == "count" {
			return value, true
		}
	case "rotational_speed":
		switch unit {
		case "revolution_per_minute":
			return value / 60, true
		case "hertz":
			return value, true
		}
	case "frequency":
		if unit == "hertz" {
			return value, true
		}
	case "current":
		if unit == "ampere" {
			return value, true
		}
	case "voltage":
		if unit == "volt" {
			return value, true
		}
	}
	return 0, false
}

func nativePointLabels(labels map[string]string) map[string]string {
	out := labelMap("protocol", labels["protocol"], "device", labels["device"], "point", labels["point"], "unit", labels["unit"], "driver_instance", labels["driver_instance"])
	if index, ok := labels["index"]; ok {
		out["index"] = index
	}
	return out
}

func writeNativeFamilySample(w *prometheusWriter, quantity string, value float64, labels map[string]string) {
	switch quantity {
	case "temperature":
		w.writeGaugeSample("helianthus_native_temperature", value, nativePointLabels(labels))
	case "temperature_difference":
		w.writeGaugeSample("helianthus_native_temperature_difference", value, nativePointLabels(labels))
	case "pressure":
		w.writeGaugeSample("helianthus_native_pressure", value, nativePointLabels(labels))
	case "volume_flow":
		w.writeGaugeSample("helianthus_native_volume_flow", value, nativePointLabels(labels))
	case "energy":
		w.writeGaugeSample("helianthus_native_energy", value, nativePointLabels(labels))
	case "power":
		w.writeGaugeSample("helianthus_native_power", value, nativePointLabels(labels))
	case "time":
		w.writeGaugeSample("helianthus_native_time", value, nativePointLabels(labels))
	case "ratio":
		w.writeGaugeSample("helianthus_native_ratio", value, nativePointLabels(labels))
	case "count":
		w.writeGaugeSample("helianthus_native_count", value, nativePointLabels(labels))
	case "rotational_speed":
		w.writeGaugeSample("helianthus_native_rotational_speed", value, nativePointLabels(labels))
	case "frequency":
		w.writeGaugeSample("helianthus_native_frequency", value, nativePointLabels(labels))
	case "current":
		w.writeGaugeSample("helianthus_native_current", value, nativePointLabels(labels))
	case "voltage":
		w.writeGaugeSample("helianthus_native_voltage", value, nativePointLabels(labels))
	}
}
