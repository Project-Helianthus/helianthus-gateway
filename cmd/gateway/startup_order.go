package main

import "github.com/Project-Helianthus/helianthus-gateway"

func shouldCloseSemanticBarrier(admissionPath ebusgateway.TransportAdmissionPath, overrideSet bool, sourceSelectionReady bool) bool {
	if admissionPath != ebusgateway.TransportAdmissionSourceSelectionCapable {
		return true
	}
	return overrideSet || sourceSelectionReady
}
