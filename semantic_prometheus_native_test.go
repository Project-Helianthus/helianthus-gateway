package ebusgateway

import (
	"strconv"
	"strings"
	"testing"
)

func TestSemanticPrometheusNativePoints(t *testing.T) {
	age := 9.0
	bar := 1.25
	rpm := 3000.0
	stale := 12.5
	points := []NativeObservation{
		{Protocol: "family.alpha", Device: "outdoor_unit", Point: "pressure", DriverInstance: "instance.a", Index: "2", Indexed: true, Quantity: "pressure", NativeUnit: "bar", NativeValue: &bar, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.alpha", Device: "outdoor_unit", Point: "pressure", DriverInstance: "instance.b", Index: "2", Indexed: true, Quantity: "pressure", NativeUnit: "bar", NativeValue: &bar, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.beta", Device: "indoor_unit", Point: "speed", DriverInstance: "instance.a", Quantity: "rotational_speed", NativeUnit: "revolution_per_minute", NativeValue: &rpm, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.beta", Device: "indoor_unit", Point: "speed", DriverInstance: "instance.b", Quantity: "rotational_speed", NativeUnit: "revolution_per_minute", NativeValue: &rpm, AgeSeconds: &age, Fresh: true},
		{Protocol: "family.alpha", Device: "meter", Point: "stale_power", DriverInstance: "instance.a", Quantity: "power", NativeUnit: "watt", NativeValue: &stale, AgeSeconds: &age, Fresh: false},
	}
	for i := 1; i <= 1025; i++ {
		value := float64(i)
		points = append(points, NativeObservation{Protocol: "family.alpha", Device: "meter", Point: "point-" + strconv.Itoa(i), DriverInstance: "instance.meter", Quantity: "power", NativeUnit: "watt", NativeValue: &value, AgeSeconds: &age, Fresh: true})
	}
	beforeBar, beforeRPM := *points[0].NativeValue, *points[2].NativeValue
	text := RenderNativePrometheus(points)
	if *points[0].NativeValue != beforeBar || points[0].NativeUnit != "bar" || *points[2].NativeValue != beforeRPM || points[2].NativeUnit != "revolution_per_minute" {
		t.Fatal("native namespace values were replaced")
	}
	for _, want := range []string{
		`helianthus_native_pressure{device="outdoor_unit",driver_instance="instance.a",index="2",point="pressure",protocol="family.alpha",unit="pascals"} 125000`,
		`helianthus_native_pressure{device="outdoor_unit",driver_instance="instance.b",index="2",point="pressure",protocol="family.alpha",unit="pascals"} 125000`,
		`helianthus_native_point_age{device="outdoor_unit",driver_instance="instance.a",index="2",point="pressure",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_point_age{device="outdoor_unit",driver_instance="instance.b",index="2",point="pressure",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_rotational_speed{device="indoor_unit",driver_instance="instance.a",point="speed",protocol="family.beta",unit="hertz"} 50`,
		`helianthus_native_rotational_speed{device="indoor_unit",driver_instance="instance.b",point="speed",protocol="family.beta",unit="hertz"} 50`,
		`helianthus_native_power{device="meter",driver_instance="instance.meter",point="point-1025",protocol="family.alpha",unit="watts"} 1025`,
		`helianthus_native_point_age{device="meter",driver_instance="instance.meter",point="point-1025",protocol="family.alpha",unit="seconds"} 9`,
		`helianthus_native_point_age{device="meter",driver_instance="instance.a",point="stale_power",protocol="family.alpha",unit="seconds"} 9`,
		"helianthus_native_render_overflow 0",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %s\n%s", want, text)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, `point="speed"`) && strings.Contains(line, `index=`) {
			t.Fatalf("unindexed point invented an index: %s", line)
		}
		if strings.Contains(line, `point="stale_power"`) && strings.Contains(line, "helianthus_native_power{") {
			t.Fatalf("stale point exported a value: %s", line)
		}
	}
	if strings.Count(text, "helianthus_native_power{") != 1025 {
		t.Fatalf("power series = %d", strings.Count(text, "helianthus_native_power{"))
	}
}
