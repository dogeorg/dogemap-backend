package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"code.dogecoin.org/dogemap-backend/internal/spec"
	"code.dogecoin.org/gossip/dnet"
	sqlite3 "github.com/mattn/go-sqlite3"
)

type NodeID = spec.NodeID
type Address = spec.Address

// SELECT * FROM table WHERE id IN (SELECT id FROM table ORDER BY RANDOM() LIMIT 10)

type SQLiteStore struct {
	db  *sql.DB
	ctx context.Context
}

var _ spec.Store = &SQLiteStore{}

// WITHOUT ROWID: SQLite version 3.8.2 (2013-12-06) or later

const SQL_SCHEMA string = `
CREATE TABLE IF NOT EXISTS migration (
	version INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS core (
	address BLOB NOT NULL PRIMARY KEY,
	time INTEGER NOT NULL,
	services INTEGER NOT NULL,
	isnew BOOLEAN NOT NULL,
	dayc INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS core_time_i ON core (time);
CREATE INDEX IF NOT EXISTS core_isnew_i ON core (isnew);
`

var MIGRATIONS = []struct {
	ver   int
	query string
}{}

// NewSQLiteStore returns a spec.Store implementation that uses SQLite
func NewSQLiteStore(fileName string, ctx context.Context) (spec.Store, error) {
	backend := "sqlite3"
	db, err := sql.Open(backend, fileName)
	store := &SQLiteStore{db: db, ctx: ctx}
	if err != nil {
		return store, dbErr(err, "opening database")
	}
	if backend == "sqlite3" {
		// limit concurrent access until we figure out a way to start transactions
		// with the BEGIN CONCURRENT statement in Go. Avoids "database locked" errors.
		db.SetMaxOpenConns(1)
	}
	err = store.initSchema()
	return store, err
}

func (s *SQLiteStore) Close() {
	s.db.Close()
}

func (s *SQLiteStore) initSchema() error {
	return s.doTxn("init schema", func(tx *sql.Tx) error {
		// apply migrations
		verRow := tx.QueryRow("SELECT version FROM migration LIMIT 1")
		var version int
		err := verRow.Scan(&version)
		if err != nil {
			// first-time database init.
			// init schema (idempotent)
			_, err := tx.Exec(SQL_SCHEMA)
			if err != nil {
				return dbErr(err, "creating database schema")
			}
			// set up version table (idempotent)
			err = tx.QueryRow("SELECT version FROM migration LIMIT 1").Scan(&version)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					version = 1
					_, err = tx.Exec("INSERT INTO migration (version) VALUES (?)", version)
					if err != nil {
						return dbErr(err, "updating version")
					}
				} else {
					return dbErr(err, "querying version")
				}
			}
		}
		initVer := version
		for _, m := range MIGRATIONS {
			if version < m.ver {
				_, err = tx.Exec(m.query)
				if err != nil {
					return dbErr(err, fmt.Sprintf("applying migration %v", m.ver))
				}
				version = m.ver
			}
		}
		if version != initVer {
			_, err = tx.Exec("UPDATE migration SET version=?", version)
			if err != nil {
				return dbErr(err, "updating version")
			}
		}
		return nil
	})
}

func (s *SQLiteStore) WithCtx(ctx context.Context) spec.Store {
	return &SQLiteStore{
		db:  s.db,
		ctx: ctx,
	}
}

// The number of whole days since the unix epoch.
func unixDayStamp() int64 {
	return time.Now().Unix() / spec.SecondsPerDay
}

func IsConflict(err error) bool {
	if sqErr, isSq := err.(sqlite3.Error); isSq {
		if sqErr.Code == sqlite3.ErrBusy || sqErr.Code == sqlite3.ErrLocked {
			return true
		}
	}
	return false
}

func (s SQLiteStore) doTxn(name string, work func(tx *sql.Tx) error) error {
	limit := 120
	for {
		tx, err := s.db.Begin()
		if err != nil {
			if IsConflict(err) {
				s.Sleep(250 * time.Millisecond)
				limit--
				if limit != 0 {
					continue
				}
			}
			return dbErr(err, "cannot begin transaction: "+name)
		}
		defer tx.Rollback()
		err = work(tx)
		if err != nil {
			if IsConflict(err) {
				s.Sleep(250 * time.Millisecond)
				limit--
				if limit != 0 {
					continue
				}
			}
			return err
		}
		err = tx.Commit()
		if err != nil {
			if IsConflict(err) {
				s.Sleep(250 * time.Millisecond)
				limit--
				if limit != 0 {
					continue
				}
			}
			return dbErr(err, "cannot commit: "+name)
		}
		return nil
	}
}

func (s SQLiteStore) Sleep(dur time.Duration) {
	select {
	case <-s.ctx.Done():
	case <-time.After(dur):
	}
}

func dbErr(err error, where string) error {
	if errors.Is(err, spec.NotFoundError) {
		return err
	}
	if sqErr, isSq := err.(sqlite3.Error); isSq {
		if sqErr.Code == sqlite3.ErrConstraint {
			// MUST detect 'AlreadyExists' to fulfil the API contract!
			// Constraint violation, e.g. a duplicate key.
			return spec.WrapErr(spec.AlreadyExists, "SQLiteStore: already-exists", err)
		}
		if sqErr.Code == sqlite3.ErrBusy || sqErr.Code == sqlite3.ErrLocked {
			// SQLite has a single-writer policy, even in WAL (write-ahead) mode.
			// SQLite will return BUSY if the database is locked by another connection.
			// We treat this as a transient database conflict, and the caller should retry.
			return spec.WrapErr(spec.DBConflict, "SQLiteStore: db-conflict", err)
		}
	}
	return spec.WrapErr(spec.DBProblem, fmt.Sprintf("SQLiteStore: db-problem: %s", where), err)
}

// STORE INTERFACE

func (s SQLiteStore) CoreStats() (mapSize int, newNodes int, err error) {
	err = s.doTxn("CoreStats", func(tx *sql.Tx) error {
		row := tx.QueryRow("WITH t AS (SELECT COUNT(address) AS num, 1 AS rn FROM core), u AS (SELECT COUNT(address) AS isnew, 1 AS rn FROM core WHERE isnew=TRUE) SELECT t.num, u.isnew FROM t INNER JOIN u ON t.rn=u.rn")
		err := row.Scan(&mapSize, &newNodes)
		if err != nil {
			// special case: always return nil (no stats) errors.
			if err != sql.ErrNoRows {
				log.Printf("[Store] CoreStats: %v", err)
			}
			return nil
		}
		return nil
	})
	return
}

func (s SQLiteStore) NodeList() (res []spec.CoreNode, err error) {
	err = s.doTxn("NodeList", func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT address,CAST(time AS INTEGER),services FROM core")
		if err != nil {
			return fmt.Errorf("[Store] coreNodeList: query: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var addr []byte
			var unixTime int64
			var services uint64
			err := rows.Scan(&addr, &unixTime, &services)
			if err != nil {
				log.Printf("[Store] coreNodeList: scanning row: %v", err)
				continue
			}
			s_adr, err := dnet.AddressFromBytes(addr)
			if err != nil {
				log.Printf("[Store] bad node address: %v", err)
				continue
			}
			res = append(res, spec.CoreNode{
				Address:  s_adr.String(),
				Time:     unixTime,
				Services: services,
			})
		}
		if err = rows.Err(); err != nil { // docs say this check is required!
			return fmt.Errorf("[Store] query: %v", err)
		}
		return nil
	})
	return
}

// TrimNodes expires records after N days.
//
// To take account of the possibility that this software has not
// been run in the last N days (which would result in immediately
// expiring all records) we use a system where:
//
// We keep a day counter that we increment once per day.
// All records, when updated, store the current day counter + N.
// Records expire once their stored day-count is < today.
//
// This causes expiry to lag by the number of offline days.
func (s SQLiteStore) TrimNodes() (advanced bool, remCore int64, err error) {
	err = s.doTxn("TrimNodes", func(tx *sql.Tx) error {
		// expire core nodes
		unixTimeSec := time.Now().Unix()
		expireBefore := unixTimeSec - spec.MaxCoreNodeDays*spec.SecondsPerDay
		res, err := tx.Exec("DELETE FROM core WHERE time < ?", expireBefore)
		if err != nil {
			return fmt.Errorf("TrimNodes: DELETE core: %v", err)
		}
		remCore, err = res.RowsAffected()
		if err != nil {
			return fmt.Errorf("TrimNodes: rows-affected: %v", err)
		}
		return nil
	})
	return
}

func (s SQLiteStore) AddCoreNode(address Address, unixTimeSec int64, services uint64) error {
	return s.doTxn("AddCoreNode", func(tx *sql.Tx) error {
		addrKey := address.ToBytes()
		res, err := tx.Exec("UPDATE core SET time=?, services=? WHERE address=?", unixTimeSec, services, addrKey)
		if err != nil {
			return fmt.Errorf("update: %v", err)
		}
		num, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows-affected: %v", err)
		}
		if num == 0 {
			_, e := tx.Exec("INSERT INTO core (address, time, services, isnew, dayc) VALUES (?1,?2,?3,true,0)",
				addrKey, unixTimeSec, services)
			if e != nil {
				return fmt.Errorf("insert: %v", e)
			}
		}
		return nil
	})
}

func (s SQLiteStore) UpdateCoreTime(address Address) (err error) {
	return s.doTxn("UpdateCoreTime", func(tx *sql.Tx) error {
		addrKey := address.ToBytes()
		unixTimeSec := time.Now().Unix()
		_, err := tx.Exec("UPDATE core SET time=? WHERE address=?", unixTimeSec, addrKey)
		if err != nil {
			return fmt.Errorf("update: %v", err)
		}
		return nil
	})
}

func (s SQLiteStore) ChooseCoreNode() (res Address, err error) {
	err = s.doTxn("ChooseCoreNode", func(tx *sql.Tx) error {
		row := tx.QueryRow("SELECT address FROM core WHERE isnew=TRUE ORDER BY RANDOM() LIMIT 1")
		var addr []byte
		err := row.Scan(&addr)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				row = tx.QueryRow("SELECT address FROM core WHERE isnew=FALSE ORDER BY RANDOM() LIMIT 1")
				err = row.Scan(&addr)
				if err != nil {
					return fmt.Errorf("query-not-new: %v", err)
				}
			} else {
				return fmt.Errorf("query-is-new: %v", err)
			}
		}
		res, err = dnet.AddressFromBytes(addr)
		if err != nil {
			return fmt.Errorf("invalid address: %v", err)
		}
		return nil
	})
	return
}
