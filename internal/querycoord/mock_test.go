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

package querycoord

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"path"
	"strconv"
	"sync"

	"google.golang.org/grpc"

	"github.com/milvus-io/milvus/internal/kv"
	minioKV "github.com/milvus-io/milvus/internal/kv/minio"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/msgstream"
	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/etcdpb"
	"github.com/milvus-io/milvus/internal/proto/indexpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/internal/proto/milvuspb"
	"github.com/milvus-io/milvus/internal/proto/proxypb"
	"github.com/milvus-io/milvus/internal/proto/querypb"
	"github.com/milvus-io/milvus/internal/proto/schemapb"
	qn "github.com/milvus-io/milvus/internal/querynode"
	"github.com/milvus-io/milvus/internal/storage"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/internal/util/funcutil"
	"github.com/milvus-io/milvus/internal/util/retry"
)

const (
	defaultCollectionID = UniqueID(2021)
	defaultPartitionID  = UniqueID(2021)
	defaultSegmentID    = UniqueID(2021)
	defaultShardsNum    = 2
)

func genCollectionSchema(collectionID UniqueID, isBinary bool) *schemapb.CollectionSchema {
	var fieldVec schemapb.FieldSchema
	if isBinary {
		fieldVec = schemapb.FieldSchema{
			FieldID:      UniqueID(101),
			Name:         "vec",
			IsPrimaryKey: false,
			DataType:     schemapb.DataType_BinaryVector,
			TypeParams: []*commonpb.KeyValuePair{
				{
					Key:   "dim",
					Value: "128",
				},
			},
			IndexParams: []*commonpb.KeyValuePair{
				{
					Key:   "metric_type",
					Value: "JACCARD",
				},
			},
		}
	} else {
		fieldVec = schemapb.FieldSchema{
			FieldID:      UniqueID(101),
			Name:         "vec",
			IsPrimaryKey: false,
			DataType:     schemapb.DataType_FloatVector,
			TypeParams: []*commonpb.KeyValuePair{
				{
					Key:   "dim",
					Value: "16",
				},
			},
			IndexParams: []*commonpb.KeyValuePair{
				{
					Key:   "metric_type",
					Value: "L2",
				},
			},
		}
	}

	return &schemapb.CollectionSchema{
		AutoID: true,
		Fields: []*schemapb.FieldSchema{
			{FieldID: 0, Name: "row_id", IsPrimaryKey: false, Description: "row_id", DataType: schemapb.DataType_Int64},
			{FieldID: 1, Name: "Ts", IsPrimaryKey: false, Description: "Ts", DataType: schemapb.DataType_Int64},
			{FieldID: 100, Name: "field_age", IsPrimaryKey: false, Description: "int64", DataType: schemapb.DataType_Int64},
			&fieldVec,
		},
	}
}

func genETCDCollectionMeta(collectionID UniqueID, isBinary bool) *etcdpb.CollectionMeta {
	schema := genCollectionSchema(collectionID, isBinary)
	collectionMeta := etcdpb.CollectionMeta{
		ID:           collectionID,
		Schema:       schema,
		CreateTime:   Timestamp(0),
		PartitionIDs: []UniqueID{defaultPartitionID},
	}

	return &collectionMeta
}

func generateInsertBinLog(collectionID UniqueID, partitionID UniqueID, segmentID UniqueID, keyPrefix string, kv kv.BaseKV) (map[int64]string, error) {
	const (
		msgLength = 1000
		DIM       = 16
	)

	idData := make([]int64, 0)
	for n := 0; n < msgLength; n++ {
		idData = append(idData, int64(n))
	}

	var timestamps []int64
	for n := 0; n < msgLength; n++ {
		timestamps = append(timestamps, int64(n+1))
	}

	var fieldAgeData []int64
	for n := 0; n < msgLength; n++ {
		fieldAgeData = append(fieldAgeData, int64(n))
	}

	fieldVecData := make([]float32, 0)
	for n := 0; n < msgLength; n++ {
		for i := 0; i < DIM; i++ {
			fieldVecData = append(fieldVecData, float32(n*i)*0.1)
		}
	}

	insertData := &storage.InsertData{
		Data: map[int64]storage.FieldData{
			0: &storage.Int64FieldData{
				NumRows: []int64{msgLength},
				Data:    idData,
			},
			1: &storage.Int64FieldData{
				NumRows: []int64{msgLength},
				Data:    timestamps,
			},
			100: &storage.Int64FieldData{
				NumRows: []int64{msgLength},
				Data:    fieldAgeData,
			},
			101: &storage.FloatVectorFieldData{
				NumRows: []int64{msgLength},
				Data:    fieldVecData,
				Dim:     DIM,
			},
		},
	}

	// buffer data to binLogs
	collMeta := genETCDCollectionMeta(collectionID, false)
	inCodec := storage.NewInsertCodec(collMeta)
	binLogs, _, err := inCodec.Serialize(partitionID, segmentID, insertData)

	if err != nil {
		log.Debug("insert data serialize error")
		return nil, err
	}

	// binLogs -> minIO/S3
	segIDStr := strconv.FormatInt(segmentID, 10)
	keyPrefix = path.Join(keyPrefix, segIDStr)

	fieldID2Paths := make(map[int64]string)
	for _, blob := range binLogs {
		uid := rand.Int63n(100000000)
		path := path.Join(keyPrefix, blob.Key, strconv.FormatInt(uid, 10))
		err = kv.Save(path, string(blob.Value[:]))
		if err != nil {
			return nil, err
		}
		fieldID, err := strconv.Atoi(blob.Key)
		if err != nil {
			return nil, err
		}
		fieldID2Paths[int64(fieldID)] = path
	}

	return fieldID2Paths, nil
}

type rootCoordMock struct {
	types.RootCoord
	CollectionIDs []UniqueID
	Col2partition map[UniqueID][]UniqueID
	sync.RWMutex
}

func newRootCoordMock() *rootCoordMock {
	collectionIDs := make([]UniqueID, 0)
	col2partition := make(map[UniqueID][]UniqueID)

	return &rootCoordMock{
		CollectionIDs: collectionIDs,
		Col2partition: col2partition,
	}
}

func (rc *rootCoordMock) createCollection(collectionID UniqueID) {
	rc.Lock()
	defer rc.Unlock()

	if _, ok := rc.Col2partition[collectionID]; !ok {
		rc.CollectionIDs = append(rc.CollectionIDs, collectionID)
		rc.Col2partition[collectionID] = make([]UniqueID, 0)
	}
}

func (rc *rootCoordMock) createPartition(collectionID UniqueID, partitionID UniqueID) error {
	rc.Lock()
	defer rc.Unlock()

	if _, ok := rc.Col2partition[collectionID]; ok {
		rc.Col2partition[collectionID] = append(rc.Col2partition[collectionID], partitionID)
	}

	return errors.New("collection not exist")
}

func (rc *rootCoordMock) ShowPartitions(ctx context.Context, in *milvuspb.ShowPartitionsRequest) (*milvuspb.ShowPartitionsResponse, error) {
	collectionID := in.CollectionID
	status := &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}
	if partitionIDs, ok := rc.Col2partition[collectionID]; ok {
		response := &milvuspb.ShowPartitionsResponse{
			Status:       status,
			PartitionIDs: partitionIDs,
		}

		return response, nil
	}

	rc.createCollection(collectionID)
	rc.createPartition(collectionID, defaultPartitionID)

	return &milvuspb.ShowPartitionsResponse{
		Status:       status,
		PartitionIDs: rc.Col2partition[collectionID],
	}, nil
}

func (rc *rootCoordMock) ReleaseDQLMessageStream(ctx context.Context, in *proxypb.ReleaseDQLMessageStreamRequest) (*commonpb.Status, error) {
	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}, nil
}

func (rc *rootCoordMock) DescribeSegment(ctx context.Context, req *milvuspb.DescribeSegmentRequest) (*milvuspb.DescribeSegmentResponse, error) {
	return nil, errors.New("describeSegment fail")
}

type dataCoordMock struct {
	types.DataCoord
	minioKV             kv.BaseKV
	collections         []UniqueID
	col2DmChannels      map[UniqueID][]*datapb.VchannelInfo
	partitionID2Segment map[UniqueID]UniqueID
	Segment2Binlog      map[UniqueID][]*datapb.SegmentBinlogs
	assignedSegmentID   UniqueID
}

func newDataCoordMock(ctx context.Context) (*dataCoordMock, error) {
	collectionIDs := make([]UniqueID, 0)
	col2DmChannels := make(map[UniqueID][]*datapb.VchannelInfo)
	partitionID2Segment := make(map[UniqueID]UniqueID)
	segment2Binglog := make(map[UniqueID][]*datapb.SegmentBinlogs)

	// create minio client
	option := &minioKV.Option{
		Address:           Params.MinioEndPoint,
		AccessKeyID:       Params.MinioAccessKeyID,
		SecretAccessKeyID: Params.MinioSecretAccessKey,
		UseSSL:            Params.MinioUseSSLStr,
		BucketName:        Params.MinioBucketName,
		CreateBucket:      true,
	}
	kv, err := minioKV.NewMinIOKV(ctx, option)
	if err != nil {
		return nil, err
	}

	return &dataCoordMock{
		minioKV:             kv,
		collections:         collectionIDs,
		col2DmChannels:      col2DmChannels,
		partitionID2Segment: partitionID2Segment,
		Segment2Binlog:      segment2Binglog,
		assignedSegmentID:   defaultSegmentID,
	}, nil
}

func (data *dataCoordMock) GetRecoveryInfo(ctx context.Context, req *datapb.GetRecoveryInfoRequest) (*datapb.GetRecoveryInfoResponse, error) {
	collectionID := req.CollectionID
	partitionID := req.PartitionID

	if _, ok := data.col2DmChannels[collectionID]; !ok {
		segmentID := data.assignedSegmentID
		data.partitionID2Segment[partitionID] = segmentID
		fieldID2Paths, err := generateInsertBinLog(collectionID, partitionID, segmentID, "queryCoorf-mockDataCoord", data.minioKV)
		if err != nil {
			return nil, err
		}
		fieldBinlogs := make([]*datapb.FieldBinlog, 0)
		for fieldID, path := range fieldID2Paths {
			fieldBinlog := &datapb.FieldBinlog{
				FieldID: fieldID,
				Binlogs: []string{path},
			}
			fieldBinlogs = append(fieldBinlogs, fieldBinlog)
		}
		data.Segment2Binlog[segmentID] = make([]*datapb.SegmentBinlogs, 0)
		segmentBinlog := &datapb.SegmentBinlogs{
			SegmentID:    segmentID,
			FieldBinlogs: fieldBinlogs,
		}
		data.Segment2Binlog[segmentID] = append(data.Segment2Binlog[segmentID], segmentBinlog)

		channelInfos := make([]*datapb.VchannelInfo, 0)
		data.collections = append(data.collections, collectionID)
		collectionName := funcutil.RandomString(8)
		for i := 0; i < defaultShardsNum; i++ {
			vChannel := fmt.Sprintf("%s_%d_%d_v", collectionName, collectionID, i)
			channelInfo := &datapb.VchannelInfo{
				CollectionID: collectionID,
				ChannelName:  vChannel,
				SeekPosition: &internalpb.MsgPosition{
					ChannelName: vChannel,
				},
				FlushedSegments: []int64{segmentID},
			}
			channelInfos = append(channelInfos, channelInfo)
		}
		data.col2DmChannels[collectionID] = channelInfos
	}

	segmentID := data.partitionID2Segment[partitionID]
	return &datapb.GetRecoveryInfoResponse{
		Status: &commonpb.Status{
			ErrorCode: commonpb.ErrorCode_Success,
		},
		Channels: data.col2DmChannels[collectionID],
		Binlogs:  data.Segment2Binlog[segmentID],
	}, nil
}

type indexCoordMock struct {
	types.IndexCoord
}

func newIndexCoordMock() *indexCoordMock {
	return &indexCoordMock{}
}

func (c *indexCoordMock) GetIndexFilePaths(ctx context.Context, req *indexpb.GetIndexFilePathsRequest) (*indexpb.GetIndexFilePathsResponse, error) {
	return nil, errors.New("get index file path fail")
}

type queryNodeServerMock struct {
	querypb.QueryNodeServer
	ctx         context.Context
	cancel      context.CancelFunc
	queryNode   *qn.QueryNode
	grpcErrChan chan error
	grpcServer  *grpc.Server

	addQueryChannels  func() (*commonpb.Status, error)
	watchDmChannels   func() (*commonpb.Status, error)
	loadSegment       func() (*commonpb.Status, error)
	releaseCollection func() (*commonpb.Status, error)
	releasePartition  func() (*commonpb.Status, error)
	releaseSegment    func() (*commonpb.Status, error)
}

func newQueryNodeServerMock(ctx context.Context) *queryNodeServerMock {
	ctx1, cancel := context.WithCancel(ctx)
	factory := msgstream.NewPmsFactory()

	return &queryNodeServerMock{
		ctx:         ctx,
		cancel:      cancel,
		queryNode:   qn.NewQueryNode(ctx1, factory),
		grpcErrChan: make(chan error),

		addQueryChannels:  returnSuccessResult,
		watchDmChannels:   returnSuccessResult,
		loadSegment:       returnSuccessResult,
		releaseCollection: returnSuccessResult,
		releasePartition:  returnSuccessResult,
		releaseSegment:    returnSuccessResult,
	}
}

func (qs *queryNodeServerMock) init() error {
	qn.Params.Init()
	qn.Params.MetaRootPath = Params.MetaRootPath
	qn.Params.QueryNodeIP = funcutil.GetLocalIP()
	grpcPort := Params.Port

	go func() {
		var lis net.Listener
		var err error

		err = retry.Do(qs.ctx, func() error {
			addr := ":" + strconv.Itoa(grpcPort)
			lis, err = net.Listen("tcp", addr)
			if err == nil {
				qn.Params.QueryNodePort = int64(lis.Addr().(*net.TCPAddr).Port)
			} else {
				// set port=0 to get next available port
				grpcPort = 0
			}
			return err
		}, retry.Attempts(10))

		if err != nil {
			log.Error(err.Error())
		}

		qs.grpcServer = grpc.NewServer()
		querypb.RegisterQueryNodeServer(qs.grpcServer, qs)
		if err = qs.grpcServer.Serve(lis); err != nil {
			log.Error(err.Error())
		}
	}()

	rootCoord := newRootCoordMock()
	indexCoord := newIndexCoordMock()
	qs.queryNode.SetRootCoord(rootCoord)
	qs.queryNode.SetIndexCoord(indexCoord)
	err := qs.queryNode.Init()
	if err != nil {
		return err
	}

	if err = qs.queryNode.Register(); err != nil {
		return err
	}

	return nil
}

func (qs *queryNodeServerMock) start() error {
	return qs.queryNode.Start()
}

func (qs *queryNodeServerMock) stop() error {
	qs.cancel()
	if qs.grpcServer != nil {
		qs.grpcServer.GracefulStop()
	}

	err := qs.queryNode.Stop()
	if err != nil {
		return err
	}

	return nil
}

func (qs *queryNodeServerMock) run() error {
	if err := qs.init(); err != nil {
		return err
	}

	if err := qs.start(); err != nil {
		return err
	}

	return nil
}

func (qs *queryNodeServerMock) AddQueryChannel(ctx context.Context, req *querypb.AddQueryChannelRequest) (*commonpb.Status, error) {
	return qs.addQueryChannels()
}

func (qs *queryNodeServerMock) WatchDmChannels(ctx context.Context, req *querypb.WatchDmChannelsRequest) (*commonpb.Status, error) {
	return qs.watchDmChannels()
}

func (qs *queryNodeServerMock) LoadSegments(ctx context.Context, req *querypb.LoadSegmentsRequest) (*commonpb.Status, error) {
	return qs.loadSegment()
}

func (qs *queryNodeServerMock) ReleaseCollection(ctx context.Context, req *querypb.ReleaseCollectionRequest) (*commonpb.Status, error) {
	return qs.releaseCollection()
}

func (qs *queryNodeServerMock) ReleasePartitions(ctx context.Context, req *querypb.ReleasePartitionsRequest) (*commonpb.Status, error) {
	return qs.releasePartition()
}

func (qs *queryNodeServerMock) ReleaseSegments(ctx context.Context, req *querypb.ReleaseSegmentsRequest) (*commonpb.Status, error) {
	return qs.releaseSegment()
}

func startQueryNodeServer(ctx context.Context) (*queryNodeServerMock, error) {
	node := newQueryNodeServerMock(ctx)
	err := node.run()
	if err != nil {
		return nil, err
	}

	return node, nil
}

func returnSuccessResult() (*commonpb.Status, error) {
	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_Success,
	}, nil
}

func returnFailedResult() (*commonpb.Status, error) {
	return &commonpb.Status{
		ErrorCode: commonpb.ErrorCode_UnexpectedError,
	}, errors.New("query node do task failed")
}
