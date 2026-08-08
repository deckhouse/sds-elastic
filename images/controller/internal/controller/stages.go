/*
Copyright 2025 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"github.com/deckhouse/sds-common-lib/conditions"

	"github.com/deckhouse/sds-elastic/api/v1alpha1"
)

// Reasons the stage FSM publishes. They are not the shared library's own —
// dashboards and alerts are keyed on these strings — so they are stated here and
// handed to it rather than adopted from it.
const (
	reasonReady          = "Ready"
	reasonInProgress     = "InProgress"
	reasonError          = "Error"
	reasonWaitingForPrev = "WaitingForPrev"
)

// stageVocabulary is what both reconcilers report their stages with.
//
// SkipMissing keeps the reading these reconcilers have always had: a stage with
// no condition is not evidence of a problem. The library defaults the other way,
// on the grounds that a stage never evaluated is not evidence of health either;
// moving to that changes what users see and belongs in a commit of its own.
var stageVocabulary = conditions.Stages{
	Passed:      reasonReady,
	Failed:      reasonError,
	InProgress:  reasonInProgress,
	Blocked:     reasonWaitingForPrev,
	SkipMissing: true,
}

// ecStages and escStages pair that vocabulary with each reconciler's stage
// order and its own aggregate condition type.
func ecStages() conditions.Stages {
	s := stageVocabulary
	s.Types = stageOrder
	s.ReadyType = v1alpha1.ECConditionReady
	return s
}

func escStages() conditions.Stages {
	s := stageVocabulary
	s.Types = escStageOrder
	s.ReadyType = v1alpha1.ESCConditionReady
	return s
}
