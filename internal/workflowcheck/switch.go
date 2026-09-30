package workflowcheck

import (
	"fmt"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
)

func vesselSwitch(api *client) error {
	formula, err := ensureApprovedFormula(api, "Switch Formula")
	if err != nil {
		return err
	}
	vesselA, err := createCheckVessel(api, "VSL-301", 240)
	if err != nil {
		return err
	}
	vesselB, err := createCheckVessel(api, "VSL-302", 240)
	if err != nil {
		return err
	}
	vesselC, err := createCheckVessel(api, "VSL-303", 240)
	if err != nil {
		return err
	}
	smallVessel, err := createCheckVessel(api, "VSL-304", 120)
	if err != nil {
		return err
	}
	spareVessel, err := createCheckVessel(api, "VSL-305", 240)
	if err != nil {
		return err
	}

	first, err := createReservedRun(api, formula.ID, vesselA.ID, "RUN-SWITCH-A")
	if err != nil {
		return err
	}
	second, err := createReservedRun(api, formula.ID, vesselB.ID, "RUN-SWITCH-B")
	if err != nil {
		return err
	}

	plannedResponse, err := api.call("POST", "/v1/runs",
		createRunPayload("RUN-SWITCH-PLANNED", formula.ID), nil)
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
	unreserved, err := api.call("POST", "/v1/runs/"+planned.ID+"/switch-vessel",
		map[string]any{"vessel_id": vesselC.ID}, ptrInt64(planned.Version))
	if err != nil {
		return err
	}
	if err := requireCode(unreserved, 409, string(domain.CodeRunState),
		"switch vessel of a run without reservation"); err != nil {
		return err
	}

	tooSmall, err := api.call("POST", "/v1/runs/"+first.Run.ID+"/switch-vessel",
		map[string]any{"vessel_id": smallVessel.ID}, ptrInt64(first.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(tooSmall, 400, string(domain.CodeInvalidInput),
		"switch to an undersized vessel"); err != nil {
		return err
	}

	occupied, err := api.call("POST", "/v1/runs/"+first.Run.ID+"/switch-vessel",
		map[string]any{"vessel_id": vesselB.ID}, ptrInt64(first.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(occupied, 409, string(domain.CodeVesselUnavailable),
		"switch to a reserved vessel"); err != nil {
		return err
	}

	missing, err := api.call("POST", "/v1/runs/"+first.Run.ID+"/switch-vessel",
		map[string]any{"vessel_id": "vessel_missing"}, ptrInt64(first.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(missing, 404, string(domain.CodeVesselNotFound),
		"switch to a missing vessel"); err != nil {
		return err
	}

	if err := requireRunVessel(api, first.Run.ID, vesselA.ID); err != nil {
		return err
	}
	if err := requireVesselOwner(api, vesselA.ID, first.Run.ID,
		domain.VesselReserved); err != nil {
		return err
	}
	if err := requireVesselOwner(api, smallVessel.ID, "", domain.VesselAvailable); err != nil {
		return err
	}

	type attempt struct {
		run      runView
		response response
		err      error
	}
	attempts := make(chan attempt, 2)
	for _, candidate := range []runView{first.Run, second.Run} {
		go func(run runView) {
			outcome, callErr := api.call("POST", "/v1/runs/"+run.ID+"/switch-vessel",
				map[string]any{"vessel_id": vesselC.ID}, ptrInt64(run.Version))
			attempts <- attempt{run: run, response: outcome, err: callErr}
		}(candidate)
	}
	outcomes := []attempt{<-attempts, <-attempts}
	var winner *switchView
	var loser runView
	for index := range outcomes {
		outcome := outcomes[index]
		if outcome.err != nil {
			return outcome.err
		}
		if outcome.response.status == 200 {
			if winner != nil {
				return errInvalidWorkflow("two concurrent switches both succeeded")
			}
			var switched switchView
			if err := outcome.response.decode(&switched); err != nil {
				return err
			}
			winner = &switched
			continue
		}
		if err := requireCode(outcome.response, 409, string(domain.CodeVesselUnavailable),
			"losing concurrent switch"); err != nil {
			return err
		}
		loser = outcome.run
	}
	if winner == nil {
		return errInvalidWorkflow("no concurrent switch succeeded")
	}
	if winner.Run.ID == loser.ID {
		return errInvalidWorkflow("concurrent switch reported the same run twice")
	}

	loserVesselID := vesselA.ID
	winnerOldVesselID := vesselB.ID
	if winner.Run.ID == first.Run.ID {
		loserVesselID = vesselB.ID
		winnerOldVesselID = vesselA.ID
	}

	if winner.Run.VesselID == nil || *winner.Run.VesselID != vesselC.ID {
		return errInvalidWorkflow("switch response did not point the run at the new vessel")
	}
	if winner.Vessel.ID != vesselC.ID ||
		winner.Vessel.ActiveRunID == nil || *winner.Vessel.ActiveRunID != winner.Run.ID {
		return errInvalidWorkflow("switch response did not assign the new vessel to the run")
	}
	if winner.ReleasedVessel.ID != winnerOldVesselID ||
		winner.ReleasedVessel.State != domain.VesselAvailable ||
		winner.ReleasedVessel.ActiveRunID != nil {
		return errInvalidWorkflow("switch response did not release the original vessel")
	}
	if winner.Run.FormulaID != formula.ID || winner.Run.PlannedVolumeL != 180.0 {
		return errInvalidWorkflow("switch changed the run formula or planned volume")
	}

	if err := requireRunVessel(api, winner.Run.ID, vesselC.ID); err != nil {
		return err
	}
	if err := requireRunVessel(api, loser.ID, loserVesselID); err != nil {
		return err
	}
	if err := requireVesselOwner(api, vesselC.ID, winner.Run.ID, domain.VesselReserved); err != nil {
		return err
	}
	if err := requireVesselOwner(api, winnerOldVesselID, "", domain.VesselAvailable); err != nil {
		return err
	}
	if err := requireVesselOwner(api, loserVesselID, loser.ID, domain.VesselReserved); err != nil {
		return err
	}

	staleStart, err := api.call("POST", "/v1/runs/"+winner.Run.ID+"/start",
		nil, ptrInt64(winner.Run.Version-1))
	if err != nil {
		return err
	}
	if err := requireCode(staleStart, 412, string(domain.CodeRevisionConflict),
		"start with a pre-switch version"); err != nil {
		return err
	}

	startResponse, err := api.call("POST", "/v1/runs/"+winner.Run.ID+"/start",
		nil, ptrInt64(winner.Run.Version))
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
	if err := requireVesselState(started.Vessel, domain.VesselInUse); err != nil {
		return err
	}
	if started.Vessel.ID != vesselC.ID {
		return errInvalidWorkflow("started run is not bound to the switched vessel")
	}

	afterStart, err := api.call("POST", "/v1/runs/"+winner.Run.ID+"/switch-vessel",
		map[string]any{"vessel_id": spareVessel.ID}, ptrInt64(started.Run.Version))
	if err != nil {
		return err
	}
	if err := requireCode(afterStart, 409, string(domain.CodeRunState),
		"switch vessel after fermentation started"); err != nil {
		return err
	}
	if err := requireVesselOwner(api, spareVessel.ID, "", domain.VesselAvailable); err != nil {
		return err
	}
	if err := requireVesselOwner(api, vesselC.ID, winner.Run.ID, domain.VesselInUse); err != nil {
		return err
	}

	loserStart, err := api.call("POST", "/v1/runs/"+loser.ID+"/start",
		nil, ptrInt64(loser.Version))
	if err != nil {
		return err
	}
	if err := requireStatus(loserStart, 200, "start run that lost the switch race"); err != nil {
		return err
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
	if err := requireStatus(runResponse, 201, "create check run"); err != nil {
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
	if err := requireStatus(reserveResponse, 200, "reserve check vessel"); err != nil {
		return pairView{}, err
	}
	var reserved pairView
	if err := reserveResponse.decode(&reserved); err != nil {
		return pairView{}, err
	}
	return reserved, nil
}

func requireRunVessel(api *client, runID string, vesselID string) error {
	response, err := api.call("GET", "/v1/runs/"+runID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(response, 200, "read run"); err != nil {
		return err
	}
	var run runView
	if err := response.decode(&run); err != nil {
		return err
	}
	if run.VesselID == nil || *run.VesselID != vesselID {
		return fmt.Errorf("run %s is not assigned to vessel %s", runID, vesselID)
	}
	return nil
}

func requireVesselOwner(
	api *client,
	vesselID string,
	runID string,
	state domain.VesselState,
) error {
	response, err := api.call("GET", "/v1/vessels/"+vesselID, nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(response, 200, "read vessel"); err != nil {
		return err
	}
	var vessel vesselView
	if err := response.decode(&vessel); err != nil {
		return err
	}
	if vessel.State != state {
		return fmt.Errorf("vessel %s has state %q, expected %q",
			vesselID, vessel.State, state)
	}
	if runID == "" {
		if vessel.ActiveRunID != nil {
			return fmt.Errorf("vessel %s still references run %s",
				vesselID, *vessel.ActiveRunID)
		}
		return nil
	}
	if vessel.ActiveRunID == nil || *vessel.ActiveRunID != runID {
		return fmt.Errorf("vessel %s is not owned by run %s", vesselID, runID)
	}
	return nil
}
