package domain

import "time"

type VesselState string

const (
	VesselAvailable VesselState = "available"
	VesselReserved  VesselState = "reserved"
	VesselInUse     VesselState = "in_use"
	VesselCleaning  VesselState = "cleaning"
	VesselRetired   VesselState = "retired"
)

type Vessel struct {
	ID          string      `json:"id"`
	Code        string      `json:"code"`
	CapacityL   float64     `json:"capacity_liters"`
	State       VesselState `json:"state"`
	ActiveRunID *string     `json:"active_run_id,omitempty"`
	Version     int64       `json:"version"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

func (v Vessel) Validate() error {
	if err := ValidateVesselCode(v.Code, "code"); err != nil {
		return err
	}
	if v.CapacityL < 10 || v.CapacityL > 5000 {
		return FieldError(CodeInvalidInput, "capacity_liters",
			"must be between 10 and 5000 liters")
	}
	return nil
}

func (v *Vessel) Reserve(runID string, now time.Time) error {
	if v.State != VesselAvailable || v.ActiveRunID != nil {
		return NewError(CodeVesselUnavailable, "vessel is not available for reservation")
	}
	run := runID
	v.State = VesselReserved
	v.ActiveRunID = &run
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) StartRun(runID string, now time.Time) error {
	if v.State != VesselReserved || v.ActiveRunID == nil || *v.ActiveRunID != runID {
		return NewError(CodeVesselState, "vessel is not reserved by this run")
	}
	v.State = VesselInUse
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) StartCleaning(runID string, now time.Time) error {
	if (v.State != VesselInUse && v.State != VesselReserved) ||
		v.ActiveRunID == nil || *v.ActiveRunID != runID {
		return NewError(CodeVesselState, "vessel cannot enter cleaning for this run")
	}
	v.State = VesselCleaning
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) FinishCleaning(now time.Time) error {
	if v.State != VesselCleaning || v.ActiveRunID == nil {
		return NewError(CodeVesselState, "vessel is not waiting for cleaning")
	}
	v.State = VesselAvailable
	v.ActiveRunID = nil
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) AcceptSwitch(runID string, now time.Time) error {
	if v.State != VesselAvailable || v.ActiveRunID != nil {
		return NewError(CodeVesselUnavailable,
			"vessel is not available to receive a switched run")
	}
	run := runID
	v.State = VesselReserved
	v.ActiveRunID = &run
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) YieldForSwitch(runID string, now time.Time) error {
	if v.State != VesselReserved || v.ActiveRunID == nil || *v.ActiveRunID != runID {
		return NewError(CodeVesselState,
			"vessel is not reserved by this run and cannot yield it")
	}
	v.State = VesselAvailable
	v.ActiveRunID = nil
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) ReleaseReservation(runID string, now time.Time) error {
	if v.State != VesselReserved && v.State != VesselInUse {
		return NewError(CodeVesselState, "vessel does not have a releasable run")
	}
	if v.ActiveRunID == nil || *v.ActiveRunID != runID {
		return NewError(CodeVesselState, "vessel belongs to a different run")
	}
	v.State = VesselAvailable
	v.ActiveRunID = nil
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}

func (v *Vessel) Retire(now time.Time) error {
	if v.State == VesselRetired {
		return NewError(CodeVesselState, "vessel is already retired")
	}
	if v.State != VesselAvailable || v.ActiveRunID != nil {
		return NewError(CodeVesselState, "only an available vessel can be retired")
	}
	v.State = VesselRetired
	v.UpdatedAt = now.UTC()
	v.Version++
	return nil
}
