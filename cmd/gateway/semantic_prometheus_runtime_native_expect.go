package main

// composedScrapeExpectation is the synthetic scrape this row declares from
// the value key, the native label set and the coverage rows. It is not
// rendered from the candidate.
var composedScrapeExpectation = []string{
	`helianthus_semantic_projection_available{domain="pv"} 1`,
	`helianthus_semantic_fact_state{availability="available",dimension="inverter",fact="pv.ac.aggregate_active_power",freshness="fresh",pack="helianthus.pack.pv",promotion="promoted",qualification="qualified",quality="good",resource="resource:inverter.synthetic",resource_kind="resource.inverter"} 1`,
	`helianthus_semantic_open_conflicts{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter"} 0`,
	`helianthus_semantic_evidence_age{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="seconds"} 1`,
	`helianthus_semantic_fact_value{dimension="inverter",fact="pv.ac.aggregate_active_power",pack="helianthus.pack.pv",resource="resource:inverter.synthetic",resource_kind="resource.inverter",unit="unit.watt"} 42`,
	`helianthus_native_pressure{device="outdoor_unit",driver_instance="instance.a",index="2",point="pressure",protocol="family.alpha",unit="pascals"} 125000`,
	`helianthus_native_pressure{device="outdoor_unit",driver_instance="instance.b",index="2",point="pressure",protocol="family.alpha",unit="pascals"} 125000`,
	`helianthus_native_point_age{device="outdoor_unit",driver_instance="instance.a",index="2",point="pressure",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_point_age{device="outdoor_unit",driver_instance="instance.b",index="2",point="pressure",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_rotational_speed{device="indoor_unit",driver_instance="instance.a",point="speed",protocol="family.beta",unit="hertz"} 50`,
	`helianthus_native_rotational_speed{device="indoor_unit",driver_instance="instance.b",point="speed",protocol="family.beta",unit="hertz"} 50`,
	`helianthus_native_point_age{device="indoor_unit",driver_instance="instance.a",point="speed",protocol="family.beta",unit="seconds"} 9`,
	`helianthus_native_point_age{device="indoor_unit",driver_instance="instance.b",point="speed",protocol="family.beta",unit="seconds"} 9`,
	`helianthus_native_point_age{device="meter",driver_instance="instance.meter",point="stale_power",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_power{device="meter",driver_instance="instance.meter",point="point-1025",protocol="family.alpha",unit="watts"} 1025`,
	`helianthus_native_point_age{device="meter",driver_instance="instance.meter",point="point-1025",protocol="family.alpha",unit="seconds"} 9`,
	`helianthus_native_render_overflow 0`,
}
