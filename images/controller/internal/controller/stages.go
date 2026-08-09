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
// SkipMissing is deliberately left off, so a stage carrying no condition
// aggregates to Unknown and the phase reads Pending. A stage that has never been
// evaluated is not evidence that the resource is healthy, and answering Ready on
// a set of verdicts that is not complete is how a resource gets called usable on
// nobody's word.
//
// It costs nothing today: every path that flushes the status has written the
// whole stage set, because advance gates the remaining stages whenever one does
// not pass. The reading only differs after a stage is added to stageOrder — the
// resources already in the cluster then report Pending until the controller has
// reconciled them once, which is the honest answer while the new stage has no
// verdict.
var stageVocabulary = conditions.Stages{
	Passed:     reasonReady,
	Failed:     reasonError,
	InProgress: reasonInProgress,
	Blocked:    reasonWaitingForPrev,
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
