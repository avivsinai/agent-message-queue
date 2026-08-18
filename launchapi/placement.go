package launchapi

import internallaunch "github.com/avivsinai/agent-message-queue/internal/launch"

func (placement PlacementV1) Validate() error {
	return toInternalPlacementValue(placement).Validate()
}

func toInternalPlacement(placement *PlacementV1) *internallaunch.Placement {
	if placement == nil {
		return nil
	}
	copied := toInternalPlacementValue(*placement)
	return &copied
}

func toInternalPlacementValue(placement PlacementV1) internallaunch.Placement {
	return internallaunch.Placement{
		Target: string(placement.Target), Layout: string(placement.Layout),
		StaggerMS: placement.StaggerMS, LauncherPane: placement.LauncherPane,
	}
}

func fromInternalPlacement(placement internallaunch.Placement) PlacementV1 {
	return PlacementV1{
		Target: PlacementTargetV1(placement.Target), Layout: PlacementLayoutV1(placement.Layout),
		StaggerMS: placement.StaggerMS, LauncherPane: placement.LauncherPane,
	}
}

func fromInternalPlacementPreview(preview internallaunch.PlacementPreview) PlacementPreviewV1 {
	result := PlacementPreviewV1{
		Effective: fromInternalPlacement(preview.Effective), Supported: preview.Supported, ReasonCode: preview.ReasonCode,
	}
	if preview.Requested != nil {
		requested := fromInternalPlacement(*preview.Requested)
		result.Requested = &requested
	}
	return result
}
