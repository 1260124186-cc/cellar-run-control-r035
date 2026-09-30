package workflowcheck

import (
	"fmt"
	"sync"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func vesselSwitch(api *client) error {
	formula, err := ensureApprovedFormula(api, "Switch Formula")
	if err != nil {
		return err
	}

	original, err := createCheckVessel(api, "VSL-SW1", 240)
	if err != nil {
		return err
	}
	target, err := createCheckVessel(api, "VSL-SW2", 300)
	if err != nil {
		return err
	}
	small, err := createCheckVessel(api, "VSL-SW3", 100)
	if err != nil {
		return err
	}
	retired, err := createCheckVessel(api, "VSL-SW4", 200)
	if err != nil {
		return err
	}
	retireResponse, err := api.call("POST", "/v1/vessels/"+retired.ID+"/retire", nil, ptrInt64(1))
	if err != nil {
		return err
	}
	if err := requireStatus(retireResponse, 200, "retire vessel"); err != nil {
		return err
	}

	reserved, err := createReservedRun(api, formula.ID, original.ID, "RUN-SWITCH")
	if err != nil {
		return err
	}
	run := reserved.Run
	preSwitchVersion := run.Version
	if run.VesselID == nil || *run.VesselID != original.ID {
		return errInvalidWorkflow("run did not start on the original vessel")
	}

	plannedResponse, err := api.call("POST", "/v1/runs",
		createRunPayload("RUN-SW-PLAN", formula.ID), nil)
	if err != nil {
		return err
	}
	if err := requireStatus(plannedResponse, 201, "create planned run"); err != nil {
		return err
	}
	var planned runView
	if err := plannedResponse.decode(&planned); err != nil {
		return err
	}

	// Every rejected attempt must leave the run and both vessels untouched.
	failureCases := []struct {
		name     string
		runID    string
		vesselID string
		version  *int64
		code     domain.Code
	}{
		{"switch to same vessel", run.ID, original.ID, ptrInt64(preSwitchVersion), domain.CodeInvalidInput},
		{"switch to undersized vessel", run.ID, small.ID, ptrInt64(preSwitchVersion), domain.CodeInvalidInput},
		{"switch to retired vessel", run.ID, retired.ID, ptrInt64(preSwitchVersion), domain.CodeVesselUnavailable},
		{"switch with stale version", run.ID, target.ID, ptrInt64(preSwitchVersion - 1), domain.CodeRevisionConflict},
		{"switch unreserved planned run", planned.ID, target.ID, ptrInt64(1), domain.CodeRunState},
	}
	for _, failure := range failureCases {
		response, callErr := api.call("POST", "/v1/runs/"+failure.runID+"/switch-vessel",
			map[string]any{"vessel_id": failure.vesselID}, failure.version)
		if callErr != nil {
			return callErr
		}
		if err := requireCode(response, statusForFailure(failure.code),
			string(failure.code), "reject "+failure.name); err != nil {
			return err
		}
	}

	missing, err := api.call("POST", "/v1/runs/"+run.ID+"/switch-vessel",
		map[string]any{"vessel_id": "vessel_does_not_exist"}, ptrInt64(preSwitchVersion))
	if err != nil {
		return err
	}
	if err := requireCode(missing, 404, string(domain.CodeVesselNotFound),
		"reject switch to missing vessel"); err != nil {
		return err
	}

	if err := requireSwitchPristine(api, run.ID, original.ID, target.ID, preSwitchVersion); err != nil {
		return err
	}

	// The controlled switch itself: run moves, old vessel yields, new vessel claims.
	switchResponse, err := api.call("POST", "/v1/runs/"+run.ID+"/switch-vessel",
		map[string]any{"vessel_id": target.ID}, ptrInt64(preSwitchVersion))
	if err != nil {
		return err
	}
	if err := requireStatus(switchResponse, 200, "switch vessel"); err != nil {
		return err
	}
	var switched switchView
	if err := switchResponse.decode(&switched); err != nil {
		return err
	}
	if err := requireRunState(switched.Run, domain.RunReserved); err != nil {
		return err
	}
	if switched.Run.VesselID == nil || *switched.Run.VesselID != target.ID {
		return errInvalidWorkflow("run was not reassigned to the new vessel")
	}
	if switched.Run.FormulaID != formula.ID {
		return errInvalidWorkflow("switch changed the run formula")
	}
	if switched.Run.PlannedVolumeL != 180.0 {
		return errInvalidWorkflow("switch changed the planned volume")
	}
	if switched.Run.Version != preSwitchVersion+1 {
		return errInvalidWorkflow("switch did not advance the run version once")
	}
	if err := requireVesselState(switched.PreviousVessel, domain.VesselAvailable); err != nil {
		return err
	}
	if switched.PreviousVessel.ActiveRunID != nil {
		return errInvalidWorkflow("previous vessel still points at the switched run")
	}
	if err := requireVesselState(switched.Vessel, domain.VesselReserved); err != nil {
		return err
	}
	if switched.Vessel.ActiveRunID == nil || *switched.Vessel.ActiveRunID != run.ID {
		return errInvalidWorkflow("new vessel does not point at the switched run")
	}

	// Persisted reads must tell the same ownership story.
	persistedRun, err := getCheckRun(api, run.ID)
	if err != nil {
		return err
	}
	if persistedRun.VesselID == nil || *persistedRun.VesselID != target.ID {
		return errInvalidWorkflow("persisted run does not name the new vessel")
	}
	persistedOld, err := getCheckVessel(api, original.ID)
	if err != nil {
		return err
	}
	persistedNew, err := getCheckVessel(api, target.ID)
	if err != nil {
		return err
	}
	if persistedOld.State != domain.VesselAvailable || persistedOld.ActiveRunID != nil {
		return errInvalidWorkflow("persisted original vessel was not released")
	}
	if persistedNew.State != domain.VesselReserved ||
		persistedNew.ActiveRunID == nil || *persistedNew.ActiveRunID != run.ID {
		return errInvalidWorkflow("persisted new vessel did not gain the run")
	}

	// The released vessel can immediately serve a different run.
	reuse, err := createReservedRun(api, formula.ID, original.ID, "RUN-SW-REUSE")
	if err != nil {
		return err
	}
	if reuse.Vessel.ActiveRunID == nil || *reuse.Vessel.ActiveRunID != reuse.Run.ID {
		return errInvalidWorkflow("released vessel could not be reserved again")
	}

	// A start based on the pre-switch version must be rejected, then the new
	// vessel carries the run into fermentation.
	staleStart, err := api.call("POST", "/v1/runs/"+run.ID+"/start", nil, ptrInt64(preSwitchVersion))
	if err != nil {
		return err
	}
	if err := requireCode(staleStart, 412, string(domain.CodeRevisionConflict),
		"reject start with pre-switch version"); err != nil {
		return err
	}
	startResponse, err := api.call("POST", "/v1/runs/"+run.ID+"/start",
		nil, ptrInt64(switched.Run.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(startResponse, 200, "start switched run"); err != nil {
		return err
	}
	var started pairView
	if err := startResponse.decode(&started); err != nil {
		return err
	}
	if err := requireRunState(started.Run, domain.RunFermenting); err != nil {
		return err
	}
	if started.Run.VesselID == nil || *started.Run.VesselID != target.ID {
		return errInvalidWorkflow("started run did not use the switched vessel")
	}
	if err := requireVesselState(started.Vessel, domain.VesselInUse); err != nil {
		return err
	}
	if started.Vessel.ActiveRunID == nil || *started.Vessel.ActiveRunID != run.ID {
		return errInvalidWorkflow("in-use vessel lost the run association")
	}

	spare, err := createCheckVessel(api, "VSL-SW5", 320)
	if err != nil {
		return err
	}
	afterStart, err := api.call("POST", "/v1/runs/"+run.ID+"/switch-vessel",
		map[string]any{"vessel_id": spare.ID}, ptrInt64(started.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(afterStart, 409, string(domain.CodeRunState),
		"reject switch after fermentation started"); err != nil {
		return err
	}

	if err := verifyConcurrentSwitch(api, formula.ID); err != nil {
		return err
	}
	return nil
}

func verifyConcurrentSwitch(api *client, formulaID string) error {
	raceTarget, err := createCheckVessel(api, "VSL-RT", 400)
	if err != nil {
		return err
	}
	vesselA, err := createCheckVessel(api, "VSL-RA", 250)
	if err != nil {
		return err
	}
	vesselB, err := createCheckVessel(api, "VSL-RB", 260)
	if err != nil {
		return err
	}
	racerA, err := createReservedRun(api, formulaID, vesselA.ID, "RUN-RACE-A")
	if err != nil {
		return err
	}
	racerB, err := createReservedRun(api, formulaID, vesselB.ID, "RUN-RACE-B")
	if err != nil {
		return err
	}

	type switchResult struct {
		response response
		err      error
	}
	results := make(chan switchResult, 2)
	var wg sync.WaitGroup
	launch := func(run runView) {
		defer wg.Done()
		resp, callErr := api.call("POST", "/v1/runs/"+run.ID+"/switch-vessel",
			map[string]any{"vessel_id": raceTarget.ID}, ptrInt64(run.Version))
		results <- switchResult{response: resp, err: callErr}
	}
	wg.Add(2)
	go launch(racerA.Run)
	go launch(racerB.Run)
	wg.Wait()
	close(results)

	winners := 0
	for result := range results {
		if result.err != nil {
			return result.err
		}
		switch result.response.status {
		case 200:
			winners++
		case 409:
			if result.response.errorCode() != string(domain.CodeVesselUnavailable) {
				return errInvalidWorkflow("losing racer returned an unexpected conflict code")
			}
		default:
			return fmt.Errorf("concurrent switch returned unexpected status %d: %s",
				result.response.status, result.response.body)
		}
	}
	if winners != 1 {
		return fmt.Errorf("expected exactly one concurrent switch to win, got %d", winners)
	}

	finalTarget, err := getCheckVessel(api, raceTarget.ID)
	if err != nil {
		return err
	}
	if finalTarget.State != domain.VesselReserved || finalTarget.ActiveRunID == nil {
		return errInvalidWorkflow("race target was not reserved by the winner")
	}
	for _, racer := range []pairView{racerA, racerB} {
		finalRun, err := getCheckRun(api, racer.Run.ID)
		if err != nil {
			return err
		}
		homeVessel, err := getCheckVessel(api, racer.Vessel.ID)
		if err != nil {
			return err
		}
		won := finalRun.VesselID != nil && *finalRun.VesselID == raceTarget.ID
		if won {
			if finalTarget.ActiveRunID == nil || *finalTarget.ActiveRunID != racer.Run.ID {
				return errInvalidWorkflow("winning run is not associated with the target vessel")
			}
			continue
		}
		if finalRun.VesselID == nil || *finalRun.VesselID != racer.Vessel.ID {
			return errInvalidWorkflow("losing run left its original vessel")
		}
		if homeVessel.State != domain.VesselReserved ||
			homeVessel.ActiveRunID == nil || *homeVessel.ActiveRunID != racer.Run.ID {
			return errInvalidWorkflow("losing racer's vessel did not stay reserved to it")
		}
	}
	return nil
}

func statusForFailure(code domain.Code) int {
	switch code {
	case domain.CodeInvalidInput:
		return 400
	case domain.CodeRunState, domain.CodeVesselUnavailable:
		return 409
	case domain.CodeRevisionConflict:
		return 412
	default:
		return 500
	}
}

func requireSwitchPristine(api *client, runID, originalID, targetID string, runVersion int64) error {
	run, err := getCheckRun(api, runID)
	if err != nil {
		return err
	}
	if run.Version != runVersion || run.State != domain.RunReserved ||
		run.VesselID == nil || *run.VesselID != originalID {
		return errInvalidWorkflow("failed switch attempts changed the run")
	}
	oldVessel, err := getCheckVessel(api, originalID)
	if err != nil {
		return err
	}
	if oldVessel.State != domain.VesselReserved ||
		oldVessel.ActiveRunID == nil || *oldVessel.ActiveRunID != runID {
		return errInvalidWorkflow("failed switch attempts released the original vessel")
	}
	newVessel, err := getCheckVessel(api, targetID)
	if err != nil {
		return err
	}
	if newVessel.State != domain.VesselAvailable || newVessel.ActiveRunID != nil {
		return errInvalidWorkflow("failed switch attempts claimed the target vessel")
	}
	return nil
}

func createReservedRun(
	api *client,
	formulaID string,
	vesselID string,
	code string,
) (pairView, error) {
	runResponse, err := api.call("POST", "/v1/runs",
		createRunPayload(code, formulaID), nil)
	if err != nil {
		return pairView{}, err
	}
	if err := requireStatus(runResponse, 201, "create switch run"); err != nil {
		return pairView{}, err
	}
	var run runView
	if err := runResponse.decode(&run); err != nil {
		return pairView{}, err
	}
	reserveResponse, err := api.call("POST", "/v1/runs/"+run.ID+"/reserve",
		map[string]any{"vessel_id": vesselID}, ptrInt64(run.Version))
	if err != nil {
		return pairView{}, err
	}
	if err := requireStatus(reserveResponse, 200, "reserve switch vessel"); err != nil {
		return pairView{}, err
	}
	var reserved pairView
	if err := reserveResponse.decode(&reserved); err != nil {
		return pairView{}, err
	}
	return reserved, nil
}

func getCheckRun(api *client, id string) (runView, error) {
	response, err := api.call("GET", "/v1/runs/"+id, nil, nil)
	if err != nil {
		return runView{}, err
	}
	if err := requireStatus(response, 200, "read run"); err != nil {
		return runView{}, err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return runView{}, err
	}
	return run, nil
}

func getCheckVessel(api *client, id string) (vesselView, error) {
	response, err := api.call("GET", "/v1/vessels/"+id, nil, nil)
	if err != nil {
		return vesselView{}, err
	}
	if err := requireStatus(response, 200, "read vessel"); err != nil {
		return vesselView{}, err
	}
	var vessel vesselView
	if err := response.decode(&vessel); err != nil {
		return vesselView{}, err
	}
	return vessel, nil
}
