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

// ecStages and escStages describe each reconciler's stage order and its own
// aggregate condition type. Everything else is left at the shared library's
// defaults, so both reconcilers report stages with the reasons every other
// storage module reports them with: Reconciled, ReconcileFailed, Pending and
// WaitingForDependency.
//
// This module used to publish Ready, Error, InProgress and WaitingForPrev
// instead. Those were kept when the FSM moved onto the shared type, on the
// grounds that something outside the module might be keyed on them; nothing is
// — the module ships no alerts or dashboards, and the strings appear in no
// chart, CRD or document. What they did do is make an ElasticCluster and, say,
// an LVMVolumeGroup describe the same situation in two vocabularies.
//
// Reasons a stage publishes for itself are not affected: a stage that reports
// WaitingForLVMVolumeGroup names something this module knows about and no
// shared vocabulary can say, so it stays.
//
// SkipMissing is deliberately left at false, so a stage carrying no condition
// aggregates to Unknown and the phase reads Pending. A stage that has never
// been evaluated is not evidence that the resource is healthy, and answering
// Ready on a set of verdicts that is not complete is how a resource gets called
// usable on nobody's word.

func ecStages() conditions.Stages {
	return conditions.Stages{
		Types:     stageOrder,
		ReadyType: v1alpha1.ECConditionReady,
	}
}

func escStages() conditions.Stages {
	return conditions.Stages{
		Types:     escStageOrder,
		ReadyType: v1alpha1.ESCConditionReady,
	}
}
