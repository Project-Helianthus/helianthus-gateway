package main

// composedRouteExpectation is the scrape this row declares from the value key
// of the served cut, the native label set and the coverage rows, written by
// hand before any scrape. It is not rendered from the candidate. Native SI
// values are worked out from the published original values: 1.25 bar is
// 125000 Pa, 20 degrees Celsius is 293.15 K, 12 L/min is 0.0002 m3/s, 2.5 kWh
// is 9000000 J, 3000 rpm is 50 Hz and 45 percent is a ratio of 0.45.
//
// The summary family listed is the per-field availability of the persisted
// store slot. The served cut carries no projection report, so the projection
// availability, item and loss samples that need one are a gap: none is listed
// and none is injected.
var composedRouteExpectation = []string{
	`helianthus_semantic_field_available{domain="thermal",field="thermal.measurement.air_temperature",slot="0"} 1`,
	`helianthus_semantic_fact_state{availability="available",dimension="thermal.dimension.zone",fact="thermal.measurement.air_temperature",freshness="fresh",pack="helianthus.pack.thermal",promotion="promoted",qualification="qualified",quality="good",resource="resource:served-zone-00",resource_kind="resource.zone"} 1`,
	`helianthus_semantic_open_conflicts{dimension="thermal.dimension.zone",fact="thermal.measurement.air_temperature",pack="helianthus.pack.thermal",resource="resource:served-zone-00",resource_kind="resource.zone"} 0`,
	`helianthus_semantic_evidence_age{dimension="thermal.dimension.zone",fact="thermal.measurement.air_temperature",pack="helianthus.pack.thermal",resource="resource:served-zone-00",resource_kind="resource.zone",unit="seconds"} 1`,
	`helianthus_semantic_fact_value{dimension="thermal.dimension.zone",fact="thermal.measurement.air_temperature",pack="helianthus.pack.thermal",resource="resource:served-zone-00",resource_kind="resource.zone",unit="unit.kelvin"} 280`,
	`helianthus_thermal_air_temperature{slot="0",unit="kelvin"} 280`,
	`helianthus_native_pressure{device="node.outdoor",driver_instance="inst.a",index="2",point="water_pressure",protocol="family.alpha",unit="pascals"} 125000`,
	`helianthus_native_pressure{device="node.outdoor",driver_instance="inst.b",index="2",point="water_pressure",protocol="family.alpha",unit="pascals"} 125000`,
	`helianthus_native_point_age{device="node.outdoor",driver_instance="inst.a",index="2",point="water_pressure",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_point_age{device="node.outdoor",driver_instance="inst.b",index="2",point="water_pressure",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_temperature{device="node.battery",driver_instance="inst.c",point="pack_temperature",protocol="family.beta",unit="kelvin"} 293.15`,
	`helianthus_native_volume_flow{device="node.battery",driver_instance="inst.c",point="coolant_flow",protocol="family.beta",unit="cubic_meters_per_second"} 0.0002`,
	`helianthus_native_energy{device="node.battery",driver_instance="inst.c",point="stored_energy",protocol="family.beta",unit="joules"} 9000000`,
	`helianthus_native_rotational_speed{device="node.battery",driver_instance="inst.c",point="fan_speed",protocol="family.beta",unit="hertz"} 50`,
	`helianthus_native_ratio{device="node.battery",driver_instance="inst.c",point="charge_level",protocol="family.beta",unit="ratio"} 0.45`,
	`helianthus_native_point_age{device="node.battery",driver_instance="inst.c",point="charge_level",protocol="family.beta",unit="seconds"} 9`,
	`helianthus_native_point_age{device="node.meter",driver_instance="inst.a",point="stale_power",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_power{device="node.meter",driver_instance="inst.a",point="point-1025",protocol="family.alpha",unit="watts"} 1025`,
	`helianthus_native_point_age{device="node.meter",driver_instance="inst.a",point="point-1025",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_render_overflow 0`,
}

// composedRouteOriginals are the values and units the drivers publish, kept
// separately from the expectation so the scrape is shown not to alter them.
var composedRouteOriginals = map[string]string{
	"water_pressure":   "1.25 unit.bar",
	"pack_temperature": "20 unit.celsius",
	"coolant_flow":     "12 unit.litre_per_minute",
	"stored_energy":    "2.5 unit.kilowatt_hour",
	"fan_speed":        "3000 unit.revolution_per_minute",
	"charge_level":     "45 unit.percent",
}
