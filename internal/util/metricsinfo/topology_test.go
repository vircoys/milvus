// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package metricsinfo

import (
	"testing"

	"github.com/milvus-io/milvus/internal/log"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/util/typeutil"

	"github.com/stretchr/testify/assert"
)

func TestConstructComponentName(t *testing.T) {
	roleList := []string{
		typeutil.QueryNodeRole,
	}
	idList := []typeutil.UniqueID{
		1,
	}

	for _, role := range roleList {
		for _, id := range idList {
			log.Info("TestConstructComponentName",
				zap.String("ComponentName", ConstructComponentName(role, id)))
		}
	}
}

func TestQueryCoordTopology_Codec(t *testing.T) {
	topology1 := QueryClusterTopology{
		Self: QueryCoordInfos{
			BaseComponentInfos: BaseComponentInfos{
				Name: ConstructComponentName(typeutil.QueryCoordRole, 1),
			},
		},
		ConnectedNodes: []QueryNodeInfos{
			{
				BaseComponentInfos: BaseComponentInfos{
					Name: ConstructComponentName(typeutil.QueryNodeRole, 1),
				},
			},
			{
				BaseComponentInfos: BaseComponentInfos{
					Name: ConstructComponentName(typeutil.QueryNodeRole, 2),
				},
			},
		},
	}
	s, err := MarshalTopology(topology1)
	assert.Equal(t, nil, err)
	log.Info("TestQueryCoordTopology_Codec",
		zap.String("marshaled_result", s))
	var topology2 QueryClusterTopology
	err = UnmarshalTopology(s, &topology2)
	assert.Equal(t, nil, err)
	assert.Equal(t, topology1.Self, topology2.Self)
	assert.Equal(t, len(topology1.ConnectedNodes), len(topology2.ConnectedNodes))
	for i := range topology1.ConnectedNodes {
		assert.Equal(t, topology1.ConnectedNodes[i], topology2.ConnectedNodes[i])
	}
}

func TestConnTopology_Codec(t *testing.T) {
	topology1 := ConnTopology{
		Name: ConstructComponentName(typeutil.ProxyRole, 1),
		ConnectedComponents: []string{
			ConstructComponentName(typeutil.RootCoordRole, 1),
			ConstructComponentName(typeutil.QueryCoordRole, 1),
			ConstructComponentName(typeutil.DataCoordRole, 1),
			ConstructComponentName(typeutil.IndexCoordRole, 1),
		},
	}
	s, err := MarshalTopology(topology1)
	assert.Equal(t, nil, err)
	log.Info("TestConnTopology_Codec",
		zap.String("marshaled_result", s))
	var topology2 ConnTopology
	err = UnmarshalTopology(s, &topology2)
	assert.Equal(t, nil, err)
	assert.Equal(t, topology1.Name, topology2.Name)
	assert.Equal(t, len(topology1.ConnectedComponents), len(topology2.ConnectedComponents))
	for i := range topology1.ConnectedComponents {
		assert.Equal(t, topology1.ConnectedComponents[i], topology2.ConnectedComponents[i])
	}
}

func TestSystemTopology_Codec(t *testing.T) {
	topology1 := SystemTopology{
		NodesInfo: []SystemTopologyNode{
			{
				Identifier: 1,
				Infos: &QueryCoordInfos{
					BaseComponentInfos: BaseComponentInfos{
						Name: ConstructComponentName(typeutil.QueryCoordRole, 1),
					},
				},
				Connected: []ConnectionEdge{
					{
						ConnectedIdentifier: 2,
						Type:                CoordConnectToNode,
						TargetType:          typeutil.QueryNodeRole,
					},
				},
			},
			{
				Identifier: 2,
				Infos: &QueryNodeInfos{
					BaseComponentInfos: BaseComponentInfos{
						Name: ConstructComponentName(typeutil.QueryNodeRole, 2),
					},
				},
				Connected: []ConnectionEdge{},
			},
		},
	}
	s, err := MarshalTopology(topology1)
	assert.Equal(t, nil, err)
	log.Info("TestSystemTopology_Codec",
		zap.String("marshaled_result", s))
	// no need to test unmarshal
}
