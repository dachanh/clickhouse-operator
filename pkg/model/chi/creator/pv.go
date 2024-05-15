// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package creator

import (
	core "k8s.io/api/core/v1"

	api "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/model/chi/namer"
	"github.com/altinity/clickhouse-operator/pkg/model/chi/tags"
)

// preparePersistentVolume prepares PV labels
func (c *Creator) preparePersistentVolume(pv *core.PersistentVolume, host *api.Host) *core.PersistentVolume {
	pv.SetLabels(namer.Macro(host).Map(c.tagger.Label(tags.LabelExistingPV, pv, host)))
	pv.SetAnnotations(namer.Macro(host).Map(c.tagger.Annotate(tags.AnnotateExistingPV, pv, host)))
	// And after the object is ready we can put version label
	tags.MakeObjectVersion(&pv.ObjectMeta, pv)
	return pv
}
