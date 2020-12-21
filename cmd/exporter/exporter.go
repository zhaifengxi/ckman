package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/ClickHouse/clickhouse-go"
	"github.com/colinmarc/hdfs/v2"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gitlab.eoitek.net/EOI/ckman/common"
)

// export data of given time range to HDFS

var (
	ckHosts  = []string{"192.168.101.106", "192.168.101.108", "192.168.101.110"}
	port     = 9000
	username = "eoi"
	password = "123456"
	tables   = map[string]string{"sensor_dt_result_online22": "@time"}
	tsBegin  = "1970-01-01 00:00:00"
	tsEnd    = "2020-11-01 00:00:00"
	tsLayout = "2006-01-02 15:04:05"

	maxFileSize  = 1e10 //10GB
	tryIntervals = []string{"1 year", "1 month", "1 week", "1 day", "4 hour", "1 hour"}
	hdfsAddr     = "192.168.101.102:8020"
	hdfsUser     = "root"
	hdfsDir      = "/user/root"

	// >=4 is able to exhaust HDFS cluster(3 DataNodes with HDDs) write bandwidth
	parallelExport = 4

	ckConns    map[string]*sql.DB
	globalPool *common.WorkerPool
	wg         sync.WaitGroup
	cntErrors  int32
	estSize    uint64
)

func initConns() (err error) {
	ckConns = make(map[string]*sql.DB)
	for _, host := range ckHosts {
		var db *sql.DB
		dsn := fmt.Sprintf("tcp://%s:%d?database=%s&username=%s&password=%s",
			host, port, "default", username, password)
		if db, err = sql.Open("clickhouse", dsn); err != nil {
			err = errors.Wrapf(err, "")
			return
		}
		ckConns[host] = db
		log.Infof("initialized clickhouse connection to %s", host)
	}
	globalPool = common.NewWorkerPool(parallelExport, len(ckHosts))
	return
}

func selectUint64(host, query string) (res uint64, err error) {
	db := ckConns[host]
	var rows *sql.Rows
	log.Infof("host %s: query: %s", host, query)
	if rows, err = db.Query(query); err != nil {
		err = errors.Wrapf(err, "")
		return
	}
	defer rows.Close()
	rows.Next()
	if err = rows.Scan(&res); err != nil {
		err = errors.Wrapf(err, "")
		return
	}
	return
}

// https://www.slideshare.net/databricks/the-parquet-format-and-performance-optimization-opportunities
// P22 sorted data helps to predicate pushdown
// P25 avoid many small files
// P27 avoid few huge files - 1GB?
func getSlots(host, table string) (slots []time.Time, err error) {
	var sizePerRow float64
	var rowsCnt uint64
	var compressed uint64
	// get size-per-row
	if rowsCnt, err = selectUint64(host, fmt.Sprintf("SELECT count() FROM %s", table)); err != nil {
		return
	}
	if rowsCnt == 0 {
		return
	}
	if compressed, err = selectUint64(host, fmt.Sprintf("SELECT sum(data_compressed_bytes) AS compressed FROM system.parts WHERE database='default' AND table='%s' AND active=1", table)); err != nil {
		return
	}
	sizePerRow = float64(compressed) / float64(rowsCnt)

	maxRowsCnt := uint64(float64(maxFileSize) / sizePerRow)
	slots = make([]time.Time, 0)
	var slot time.Time
	db := ckConns[host]

	tsColumn := tables[table]
	var totalRowsCnt uint64
	if totalRowsCnt, err = selectUint64(host, fmt.Sprintf("SELECT count() FROM %s WHERE `%s`>='%s' AND `%s`<'%s'", table, tsColumn, tsBegin, tsColumn, tsEnd)); err != nil {
		return
	}
	tblEstSize := totalRowsCnt * uint64(sizePerRow)
	log.Infof("host %s: totol rows to export: %d, estimated size (in bytes): %d", host, totalRowsCnt, tblEstSize)
	atomic.AddUint64(&estSize, tblEstSize)

	sqlTmpl3 := "SELECT toStartOfInterval(`%s`, INTERVAL %s) AS slot, count() FROM %s WHERE `%s`>='%s' AND `%s`<'%s' GROUP BY slot ORDER BY slot"
	for i, interval := range tryIntervals {
		slots = slots[:0]
		var rows1 *sql.Rows
		query1 := fmt.Sprintf(sqlTmpl3, tsColumn, interval, table, tsColumn, tsBegin, tsColumn, tsEnd)
		log.Infof("host %s: query: %s", host, query1)
		if rows1, err = db.Query(query1); err != nil {
			err = errors.Wrapf(err, "")
			return
		}
		defer rows1.Close()
		var tooBigSlot bool
	LOOP_RS:
		for rows1.Next() {
			if err = rows1.Scan(&slot, &rowsCnt); err != nil {
				err = errors.Wrapf(err, "")
				return
			}
			if rowsCnt > maxRowsCnt && i != len(tryIntervals)-1 {
				tooBigSlot = true
				break LOOP_RS
			}
			slots = append(slots, slot)
		}
		if !tooBigSlot {
			break
		}
	}
	return
}

func export(host, table string, slots []time.Time) {
	var err error
	for i := 0; i < len(slots); i++ {
		var slotBeg, slotEnd time.Time
		slotBeg = slots[i]
		if i != len(slots)-1 {
			slotEnd = slots[i+1]
		} else {
			if slotEnd, err = time.Parse(tsLayout, tsEnd); err != nil {
				panic(fmt.Sprintf("BUG: failed to parse %s, layout %s", tsEnd, tsLayout))
			}
		}
		exportSlot(host, table, i, slotBeg, slotEnd)
	}
	return
}

func exportSlot(host, table string, seq int, slotBeg, slotEnd time.Time) {
	tsColumn := tables[table]
	wg.Add(1)
	globalPool.Submit(func() {
		defer wg.Done()
		hdfsTbl := "hdfs_" + table + "_" + slotBeg.Format("20060102150405")
		fp := filepath.Join(hdfsDir, table+"_"+host+"_"+slotBeg.Format("20060102150405")+".parquet")
		queries := []string{
			fmt.Sprintf("DROP TABLE IF EXISTS %s", hdfsTbl),
			fmt.Sprintf("CREATE TABLE %s AS %s ENGINE=HDFS('hdfs://%s%s', 'Parquet')", hdfsTbl, table, hdfsAddr, fp),
			fmt.Sprintf("INSERT INTO %s SELECT * FROM %s WHERE `%s`>='%s' AND `%s`<'%s'", hdfsTbl, table, tsColumn, slotBeg.Format(tsLayout), tsColumn, slotEnd.Format(tsLayout)),
			fmt.Sprintf("DROP TABLE %s", hdfsTbl),
		}
		db := ckConns[host]
		for _, query := range queries {
			if atomic.LoadInt32(&cntErrors) != 0 {
				return
			}
			log.Infof("host %s, table %s, slot %d, query: %s", host, table, seq, query)
			if _, err := db.Exec(query); err != nil {
				log.Errorf("host %s: got error %+v", host, err)
				atomic.AddInt32(&cntErrors, 1)
				return
			}
		}
		log.Infof("host %s, table %s, slot %d, export done", host, table, seq)
	})
}

func main() {
	var err error
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	if err = initConns(); err != nil {
		log.Fatalf("got error %+v", err)
	}

	var tBeg, tEnd time.Time
	if tBeg, err = time.Parse(tsLayout, tsBegin); err != nil {
		panic(fmt.Sprintf("BUG: failed to parse %s, layout %s", tsBegin, tsLayout))
	}
	if tEnd, err = time.Parse(tsLayout, tsEnd); err != nil {
		panic(fmt.Sprintf("BUG: failed to parse %s, layout %s", tsEnd, tsLayout))
	}
	dir := tBeg.Format("20060102150405") + "_" + tEnd.Format("20060102150405")
	hdfsDir = filepath.Join(hdfsDir, dir)
	var hc *hdfs.Client
	if hc, err = hdfs.New(hdfsAddr); err != nil {
		err = errors.Wrapf(err, "")
		log.Fatalf("got error %+v", err)
	}
	if err = hc.RemoveAll(hdfsDir); err != nil {
		err = errors.Wrapf(err, "")
		log.Fatalf("got error %+v", err)
	}
	if err = hc.MkdirAll(hdfsDir, 0777); err != nil {
		err = errors.Wrapf(err, "")
		log.Fatalf("got error %+v", err)
	}

	t0 := time.Now()
	for i := 0; i < len(ckHosts); i++ {
		host := ckHosts[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			var slots []time.Time
			for table := range tables {
				if slots, err = getSlots(host, table); err != nil {
					log.Fatalf("host %s: got error %+v", host, err)
				}
				export(host, table, slots)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&cntErrors) != 0 {
		log.Errorf("export failed")
	} else {
		du := uint64(time.Since(t0).Seconds())
		size := atomic.LoadUint64(&estSize)
		msg := fmt.Sprintf("exported %d bytes in %d seconds", size, du)
		if du != 0 {
			msg += fmt.Sprintf(", %d bytes/s", size/du)
		}
		log.Infof(msg)
	}
	return
}
