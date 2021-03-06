package tileserver

import (
	"container/list"
	"sync"
	"time"

	"github.com/op/go-logging"
	"github.com/orofarne/hmetrics2"

	"app"
	"gopnik"
	"gopnikrpc"
	"perflog"
	"tilerender"
)

var log = logging.MustGetLogger("global")

var hReqT = hmetrics2.MustRegisterPackageMetric("request_time", hmetrics2.NewHistogram()).(*hmetrics2.Histogram)
var hReqOk = hmetrics2.MustRegisterPackageMetric("request_ok", hmetrics2.NewCounter()).(*hmetrics2.Counter)
var hReqErr = hmetrics2.MustRegisterPackageMetric("request_err", hmetrics2.NewCounter()).(*hmetrics2.Counter)

type TileServer struct {
	renders     *tilerender.MultiRenderPool
	cache       gopnik.CachePluginInterface
	saveList    *list.List
	saveListMu  sync.RWMutex
	removeDelay time.Duration
}

type saveQueueElem struct {
	gopnik.TileCoord
	Data []gopnik.Tile
}

func NewTileServer(poolsCfg app.RenderPoolsConfig, cp gopnik.CachePluginInterface, removeDelay time.Duration) (*TileServer, error) {
	self := &TileServer{
		cache:       cp,
		saveList:    list.New(),
		removeDelay: removeDelay,
	}

	var err error
	self.renders, err = tilerender.NewMultiRenderPool(poolsCfg)

	return self, err
}

func (self *TileServer) cacheMetatile(tc *gopnik.TileCoord, tiles []gopnik.Tile) error {
	listElem := self.saveQueuePut(tc, tiles)

	defer func() {
		go func() {
			time.Sleep(self.removeDelay)
			self.saveQueueRemove(listElem)
		}()
	}()

	err := self.cache.Set(*tc, tiles)

	if err != nil {
		log.Error("Cache write error: %v", err)
	}

	return err
}

func (self *TileServer) saveQueuePut(coord *gopnik.TileCoord, tiles []gopnik.Tile) *list.Element {
	self.saveListMu.Lock()
	defer self.saveListMu.Unlock()

	elem := saveQueueElem{
		TileCoord: *coord,
		Data:      tiles,
	}
	return self.saveList.PushFront(&elem)
}

func (self *TileServer) saveQueueRemove(elem *list.Element) {
	self.saveListMu.Lock()
	defer self.saveListMu.Unlock()

	self.saveList.Remove(elem)
}

func (self *TileServer) saveQueueGet(coord *gopnik.TileCoord) []gopnik.Tile {
	self.saveListMu.RLock()
	defer self.saveListMu.RUnlock()

	for e := self.saveList.Front(); e != nil; e = e.Next() {
		elem := e.Value.(*saveQueueElem)
		if elem.Equals(coord) {
			return elem.Data
		}
	}
	return nil
}

func (self *TileServer) checkSaveQueue(coord *gopnik.TileCoord) []gopnik.Tile {
	metacoord := app.App.Metatiler().TileToMetatile(coord)

	return self.saveQueueGet(&metacoord)
}

func (self *TileServer) ServeTileRequest(tc *gopnik.TileCoord, prio gopnikrpc.Priority, wait_storage bool) (tiles []gopnik.Tile, renderTime, saveTime time.Duration, err error) {
	τ0 := time.Now()

	tiles, renderTime, saveTime, err = self.serveTileRequest(tc, prio, wait_storage)

	// Statistics
	hReqT.AddPoint(time.Since(τ0).Seconds())
	if err == nil {
		hReqOk.Inc()
	} else {
		hReqErr.Inc()
	}

	// save to perflog
	perflog.SavePerf(perflog.PerfLogEntry{
		Timestamp:  time.Now(),
		Coord:      *tc,
		RenderTime: renderTime,
		SaverTime:  saveTime,
	})

	return
}

func (self *TileServer) serveTileRequest(tc *gopnik.TileCoord, prio gopnikrpc.Priority, wait_storage bool) (tiles []gopnik.Tile, renderTime, saveTime time.Duration, err error) {
	metacoord := app.App.Metatiler().TileToMetatile(tc)

	if tc.Size != 0 && tc.Size != 1 && tc.Size != metacoord.Size {
		return nil, 0, 0, &gopnikrpc.RenderError{Message: "Invalid tile size"}
	}

	if tiles = self.checkSaveQueue(tc); tiles == nil {
		ansCh := make(chan *tilerender.RenderPoolResponse)

		τ0 := time.Now()
		if err = self.renders.EnqueueRequest(metacoord, ansCh, prio); err != nil {
			return nil, 0, 0, &gopnikrpc.QueueLimitExceeded{}
		}

		ans := <-ansCh
		renderTime = time.Since(τ0)
		if ans.Error != nil {
			return nil, 0, 0, &gopnikrpc.RenderError{Message: ans.Error.Error()}
		}

		if wait_storage {
			τ1 := time.Now()
			err := self.cacheMetatile(&metacoord, ans.Tiles)
			saveTime = time.Since(τ1)
			if err != nil {
				return nil, 0, 0, &gopnikrpc.RenderError{Message: err.Error()}
			}
		} else {
			go self.cacheMetatile(&metacoord, ans.Tiles)
		}

		tiles = ans.Tiles
	}

	if tc.Size == 0 {
		return nil, renderTime, saveTime, nil
	}

	if tc.Size == metacoord.Size {
		return tiles, renderTime, saveTime, nil
	}

	index := (tc.Y-metacoord.Y)*metacoord.Size + (tc.X - metacoord.X)
	return []gopnik.Tile{tiles[index]}, renderTime, saveTime, nil
}

func (self *TileServer) ReloadStyle() error {
	self.renders.Reload()
	return nil
}
