package unmarshaller

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	logging "github.com/op/go-logging"
	"gitlab.yunshan.net/yunshan/droplet-libs/zerodoc"

	"gitlab.yunshan.net/yunshan/droplet-libs/app"
	"gitlab.yunshan.net/yunshan/droplet-libs/codec"
	"gitlab.yunshan.net/yunshan/droplet-libs/grpc"
	"gitlab.yunshan.net/yunshan/droplet-libs/queue"
	"gitlab.yunshan.net/yunshan/droplet-libs/receiver"
	"gitlab.yunshan.net/yunshan/droplet-libs/stats"
	"gitlab.yunshan.net/yunshan/droplet-libs/utils"
	"gitlab.yunshan.net/yunshan/droplet-libs/zerodoc/pb"
	"gitlab.yunshan.net/yunshan/droplet/roze/dbwriter"
)

var log = logging.MustGetLogger("roze.unmarshaller")

const (
	QUEUE_BATCH_SIZE = 1024
	FLUSH_INTERVAL   = 5
	GET_MAX_SIZE     = 1024
	DOC_TIME_EXCEED  = 300
	HASH_SEED        = 17
)

type QueueCache struct {
	values []interface{}
}

type Counter struct {
	DocCount        int64 `statsd:"doc-count"`
	ErrDocCount     int64 `statsd:"err-doc-count"`
	AverageDelay    int64 `statsd:"average-delay"`
	MaxDelay        int64 `statsd:"max-delay"`
	MinDelay        int64 `statsd:"min-delay"`
	ExpiredDocCount int64 `statsd:"expired-doc-count"`
	FutureDocCount  int64 `statsd:"future-doc-count"`
	DropDocCount    int64 `statsd:"drop-doc-count"`

	FlowPortCount       int64 `statsd:"vtap-flow-port"`
	FlowPort1sCount     int64 `statsd:"vtap-flow-port-1s"`
	FlowEdgePortCount   int64 `statsd:"vtap-flow-edge-port"`
	FlowEdgePort1sCount int64 `statsd:"vtap-flow-edge-port-1s"`
	AclCount            int64 `statsd:"vtap-acl"`
	OtherCount          int64 `statsd:"other-db-count"`
}

type Unmarshaller struct {
	index              int
	platformData       *grpc.PlatformInfoTable
	disableSecondWrite bool
	unmarshallQueue    queue.QueueReader
	dbwriter           *dbwriter.DbWriter
	queueBatchCache    QueueCache
	counter            *Counter
	dbCounter          [zerodoc.VTAP_DB_ID_MAX + 1]int64
	utils.Closable
}

func NewUnmarshaller(index int, platformData *grpc.PlatformInfoTable, disableSecondWrite bool, unmarshallQueue queue.QueueReader, dbwriter *dbwriter.DbWriter) *Unmarshaller {
	return &Unmarshaller{
		index:              index,
		platformData:       platformData,
		disableSecondWrite: disableSecondWrite,
		unmarshallQueue:    unmarshallQueue,
		counter:            &Counter{MaxDelay: -3600, MinDelay: 3600},
		dbwriter:           dbwriter,
	}
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (u *Unmarshaller) isGoodDocument(docTime int64) bool {
	delay := time.Now().Unix() - docTime
	u.counter.DocCount++
	u.counter.AverageDelay += delay
	u.counter.MaxDelay = max(u.counter.MaxDelay, delay)
	u.counter.MinDelay = min(u.counter.MinDelay, delay)
	if delay > DOC_TIME_EXCEED {
		u.counter.ExpiredDocCount++
		return false
	}
	if delay < -DOC_TIME_EXCEED {
		u.counter.FutureDocCount++
		return false
	}
	return true
}

func (u *Unmarshaller) GetCounter() interface{} {
	var counter *Counter
	counter, u.counter = u.counter, &Counter{MaxDelay: -3600, MinDelay: 3600}

	if counter.DocCount != 0 {
		counter.AverageDelay /= counter.DocCount
	} else {
		counter.MaxDelay = 0
		counter.MinDelay = 0
	}

	counter.FlowPortCount, u.dbCounter[zerodoc.VTAP_FLOW_PORT] = u.dbCounter[zerodoc.VTAP_FLOW_PORT], 0
	counter.FlowPort1sCount, u.dbCounter[zerodoc.VTAP_FLOW_PORT_1S] = u.dbCounter[zerodoc.VTAP_FLOW_PORT_1S], 0
	counter.FlowEdgePortCount, u.dbCounter[zerodoc.VTAP_FLOW_EDGE_PORT] = u.dbCounter[zerodoc.VTAP_FLOW_EDGE_PORT], 0
	counter.FlowEdgePort1sCount, u.dbCounter[zerodoc.VTAP_FLOW_EDGE_PORT_1S] = u.dbCounter[zerodoc.VTAP_FLOW_EDGE_PORT_1S], 0
	counter.AclCount, u.dbCounter[zerodoc.VTAP_ACL] = u.dbCounter[zerodoc.VTAP_ACL], 0
	counter.OtherCount, u.dbCounter[zerodoc.VTAP_DB_ID_MAX] = u.dbCounter[zerodoc.VTAP_DB_ID_MAX], 0

	return counter
}

func (u *Unmarshaller) putStoreQueue(doc *app.Document) {
	queueCache := &u.queueBatchCache
	queueCache.values = append(queueCache.values, doc)

	if len(queueCache.values) >= QUEUE_BATCH_SIZE {
		u.dbwriter.Put(queueCache.values...)
		queueCache.values = queueCache.values[:0]
	}
}

func (u *Unmarshaller) flushStoreQueue() {
	queueCache := &u.queueBatchCache
	if len(queueCache.values) > 0 {
		u.dbwriter.Put(queueCache.values...)
		queueCache.values = queueCache.values[:0]
	}
}

func DecodeForQueueMonitor(item interface{}) (interface{}, error) {
	var ret interface{}
	bytes, ok := item.(*receiver.RecvBuffer)
	if !ok {
		return nil, errors.New("only support data(type: RecvBuffer) to unmarshall")
	}

	ret, err := decodeForDebug(bytes.Buffer[bytes.Begin:bytes.End])
	return ret, err
}

type BatchDocument []*app.Document

func (bd BatchDocument) String() string {
	docs := []*app.Document(bd)
	str := fmt.Sprintf("batch msg num=%d\n", len(docs))
	for i, doc := range docs {
		str += fmt.Sprintf("%d%s", i, doc.String())
	}
	return str
}

func decodeForDebug(b []byte) (BatchDocument, error) {
	if b == nil {
		return nil, errors.New("No input byte")
	}

	decoder := &codec.SimpleDecoder{}
	decoder.Init(b)
	docs := make([]*app.Document, 0)

	for !decoder.IsEnd() {
		doc, err := app.DecodeForQueueMonitor(decoder)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return BatchDocument(docs), nil
}

func (u *Unmarshaller) QueueProcess() {
	stats.RegisterCountable("unmarshaller", u, stats.OptionStatTags{"thread": strconv.Itoa(u.index)})
	rawDocs := make([]interface{}, GET_MAX_SIZE)
	decoder := &codec.SimpleDecoder{}
	pbDoc := pb.NewDocument()
	for {
		n := u.unmarshallQueue.Gets(rawDocs)
		for i := 0; i < n; i++ {
			value := rawDocs[i]
			if recvBytes, ok := value.(*receiver.RecvBuffer); ok {
				bytes := recvBytes.Buffer[recvBytes.Begin:recvBytes.End]
				decoder.Init(bytes)
				for !decoder.Failed() && !decoder.IsEnd() {
					pbDoc.ResetAll()
					doc, err := app.DecodePB(decoder, pbDoc)
					if err != nil {
						u.counter.ErrDocCount++
						log.Warningf("Decode failed, bytes len=%d err=%s", len([]byte(bytes)), err)
						break
					}
					u.isGoodDocument(int64(doc.Timestamp))

					// 秒级数据是否写入
					if u.disableSecondWrite &&
						doc.Flags&app.FLAG_PER_SECOND_METRICS != 0 {
						app.ReleaseDocument(doc)
						continue
					}

					if err := DocumentExpand(doc, u.platformData); err != nil {
						log.Debug(err)
						u.counter.DropDocCount++
						app.ReleaseDocument(doc)
						continue
					}

					tableID, err := doc.TableID()
					if err != nil {
						log.Debug(err)
						u.counter.DropDocCount++
						app.ReleaseDocument(doc)
						continue
					}
					u.dbCounter[tableID]++

					u.putStoreQueue(doc)
				}
				receiver.ReleaseRecvBuffer(recvBytes)

			} else if value == nil { // flush ticker
				u.flushStoreQueue()
			} else {
				log.Warning("get unmarshall queue data type wrong")
			}
		}
	}
}
