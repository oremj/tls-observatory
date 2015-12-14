package database

import (
	"fmt"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/lib/pq"

	"github.com/mozilla/tls-observatory/logger"
)

// RegisterScanListener "subscribes" to the notifications published to the scan_listener notifier.
// It has as input the usual sb attributes and returns an int64 channel which can be consumed
// for newly created scan id's.
func (db *DB) RegisterScanListener(dbname, user, password, hostport, sslmode string) <-chan int64 {

	log := logger.GetLogger()

	reportProblem := func(ev pq.ListenerEventType, err error) {
		if err != nil {
			log.WithFields(logrus.Fields{
				"error": err.Error(),
			}).Error("Listener Error")
		}
	}

	listenerChan := make(chan int64)
	listenerName := "scan_listener"

	connInfo := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s",
		user, password, hostport, dbname, sslmode)

	go func() {

		listener := pq.NewListener(connInfo, 100*time.Millisecond, 10*time.Second, reportProblem)
		err := listener.Listen(listenerName)

		if err != nil {
			log.WithFields(logrus.Fields{
				"listener": listenerName,
				"error":    err.Error(),
			}).Error("could not listen for notification")
			close(listenerChan)
			return
		}

		for m := range listener.Notify {
			sid := m.Extra
			if db.acquireNotification(sid) {

				id, err := strconv.ParseInt(string(sid), 10, 64)
				if err != nil {
					log.WithFields(logrus.Fields{
						"scan_id": sid,
						"error":   err.Error(),
					}).Error("could not decode acquired notification")
				}

				listenerChan <- id

				log.WithFields(logrus.Fields{
					"scan_id": id,
				}).Debug("Acquired notification.")
			}
		}

	}()

	go func() {
		fiveminticker := time.NewTicker(1 * time.Minute)
		unackedQuery := fmt.Sprintf("select pg_notify('%s', ''||id ) from scans where ack=FALSE and timestamp < NOW() - INTERVAL '30 seconds'", listenerName)
		zerocomplQuery := "update scans set ack=false where completion_perc=0 and timestamp < NOW() - INTERVAL '1 minute'"
		for {
			select {
			case <-fiveminticker.C:
				_, err := db.Exec(zerocomplQuery)
				if err != nil {
					log.WithFields(logrus.Fields{
						"error": err,
					}).Error("Could not run zero completion update query")
				}

				_, err = db.Exec(unackedQuery)
				if err != nil {
					log.WithFields(logrus.Fields{
						"error": err,
					}).Error("Could not run unacknowledged scans periodic check.")
				}
			}
		}
	}()

	return listenerChan
}

func (db *DB) acquireNotification(id string) bool {
	tx, err := db.Begin()
	if err != nil {
		return false
	}
	// `ack` is a mutex in the database that each scanner will try to select
	// for update. if a scanner succeeds, it will return true, otherwise it
	// will return false.
	row := tx.QueryRow("SELECT ack FROM scans WHERE id=$1 FOR UPDATE", id)

	ack := false
	err = row.Scan(&ack)
	if err != nil {
		tx.Rollback()
		return false
	}
	if !ack {
		_, err = tx.Exec("UPDATE scans SET ack=$1 WHERE id=$2", true, id)
		if err != nil {
			tx.Rollback()
			return false
		}
		err = tx.Commit()
		if err != nil {
			tx.Rollback()
			return false
		}
		return true
	}
	tx.Rollback()
	return false
}
