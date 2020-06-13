package gue

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgtype"
	"github.com/jackc/pgx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockJob(t *testing.T) {
	c := openTestClient(t)

	jobType := "MyJob"
	err := c.Enqueue(&Job{Type: jobType})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)

	require.NotNil(t, j.conn)
	require.NotNil(t, j.pool)
	defer j.Done()

	// check values of returned Job
	assert.Greater(t, j.ID, int64(0))
	assert.Equal(t, defaultQueueName, j.Queue)
	assert.Equal(t, int16(100), j.Priority)
	assert.False(t, j.RunAt.IsZero())
	assert.Equal(t, jobType, j.Type)
	assert.Equal(t, []byte(`[]`), j.Args)
	assert.Equal(t, int32(0), j.ErrorCount)
	assert.NotEqual(t, pgtype.Present, j.LastError.Status)

	// check for advisory lock
	var count int64
	query := "SELECT count(*) FROM pg_locks WHERE locktype=$1 AND objid=$2::bigint"
	err = j.pool.QueryRow(query, "advisory", j.ID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// make sure conn was checked out of pool
	stat := c.pool.Stat()
	total, available := stat.CurrentConnections, stat.AvailableConnections
	assert.Equal(t, total-1, available)

	err = j.Delete()
	require.NoError(t, err)
}

func TestLockJobAlreadyLocked(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)
	defer j.Done()

	j2, err := c.LockJob("")
	require.NoError(t, err)

	if j2 != nil {
		defer j2.Done()
		require.Fail(t, "wanted no job, got %+v", j2)
	}
}

func TestLockJobNoJob(t *testing.T) {
	c := openTestClient(t)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.Nil(t, j)
}

func TestLockJobCustomQueue(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob", Queue: "extra_priority"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	if j != nil {
		j.Done()
		assert.Fail(t, "expected no job to be found with empty queue name, got %+v", j)
	}

	j, err = c.LockJob("extra_priority")
	require.NoError(t, err)
	defer j.Done()
	require.NotNil(t, j)

	err = j.Delete()
	require.NoError(t, err)
}

func TestJobConn(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)
	defer j.Done()

	assert.Equal(t, j.conn, j.Conn())
}

func TestJobConnRace(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)
	defer j.Done()

	var wg sync.WaitGroup
	wg.Add(2)

	// call Conn and Done in different goroutines to make sure they are safe from
	// races.
	go func() {
		_ = j.Conn()
		wg.Done()
	}()
	go func() {
		j.Done()
		wg.Done()
	}()
	wg.Wait()
}

// Test the race condition in LockJob
func TestLockJobAdvisoryRace(t *testing.T) {
	c := openTestClientMaxConns(t, 2)

	// *pgx.ConnPool doesn't support pools of only one connection.  Make sure
	// the other one is busy so we know which backend will be used by LockJob
	// below.
	unusedConn, err := c.pool.Acquire()
	require.NoError(t, err)
	defer c.pool.Release(unusedConn)

	// We use two jobs: the first one is concurrently deleted, and the second
	// one is returned by LockJob after recovering from the race condition.
	for i := 0; i < 2; i++ {
		err := c.Enqueue(&Job{Type: "MyJob"})
		require.NoError(t, err)
	}

	// helper functions
	newConn := func() *pgx.Conn {
		conn, err := pgx.Connect(testConnConfig(t))
		require.NoError(t, err)
		return conn
	}

	getBackendPID := func(conn *pgx.Conn) int32 {
		var backendPID int32
		err := conn.QueryRow(`SELECT pg_backend_pid()`).Scan(&backendPID)
		require.NoError(t, err)
		return backendPID
	}

	waitUntilBackendIsWaiting := func(backendPID int32, name string) {
		conn := newConn()
		i := 0
		for {
			var waiting bool
			err := conn.QueryRow(`SELECT wait_event is not null from pg_stat_activity where pid=$1`, backendPID).Scan(&waiting)
			require.NoError(t, err)

			if waiting {
				break
			} else {
				i++
				if i >= 10000/50 {
					panic(fmt.Sprintf("timed out while waiting for %s", name))
				}

				time.Sleep(50 * time.Millisecond)
			}
		}

	}

	// Reproducing the race condition is a bit tricky.  The idea is to form a
	// lock queue on the relation that looks like this:
	//
	//   AccessExclusive <- AccessShare  <- AccessExclusive ( <- AccessShare )
	//
	// where the leftmost AccessShare lock is the one implicitly taken by the
	// sqlLockJob query.  Once we release the leftmost AccessExclusive lock
	// without releasing the rightmost one, the session holding the rightmost
	// AccessExclusiveLock can run the necessary DELETE before the sqlCheckJob
	// query runs (since it'll be blocked behind the rightmost AccessExclusive
	// Lock).
	//
	deletedJobIDChan := make(chan int64, 1)
	lockJobBackendIDChan := make(chan int32)
	secondAccessExclusiveBackendIDChan := make(chan int32)

	go func() {
		conn := newConn()
		defer func() {
			err := conn.Close()
			assert.NoError(t, err)
		}()

		tx, err := conn.Begin()
		require.NoError(t, err)

		_, err = tx.Exec(`LOCK TABLE que_jobs IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)

		// first wait for LockJob to appear behind us
		backendID := <-lockJobBackendIDChan
		waitUntilBackendIsWaiting(backendID, "LockJob")

		// then for the AccessExclusive lock to appear behind that one
		backendID = <-secondAccessExclusiveBackendIDChan
		waitUntilBackendIsWaiting(backendID, "second access exclusive lock")

		err = tx.Rollback()
		require.NoError(t, err)
	}()

	go func() {
		conn := newConn()
		defer func() {
			err := conn.Close()
			assert.NoError(t, err)
		}()

		// synchronization point
		secondAccessExclusiveBackendIDChan <- getBackendPID(conn)

		tx, err := conn.Begin()
		require.NoError(t, err)

		_, err = tx.Exec(`LOCK TABLE que_jobs IN ACCESS EXCLUSIVE MODE`)
		require.NoError(t, err)

		// Fake a concurrent transaction grabbing the job
		var jid int64
		err = tx.QueryRow(`
			DELETE FROM que_jobs
			WHERE job_id =
				(SELECT min(job_id)
				 FROM que_jobs)
			RETURNING job_id
		`).Scan(&jid)
		require.NoError(t, err)

		deletedJobIDChan <- jid

		err = tx.Commit()
		require.NoError(t, err)
	}()

	conn, err := c.pool.Acquire()
	require.NoError(t, err)

	ourBackendID := getBackendPID(conn)
	c.pool.Release(conn)

	// synchronization point
	lockJobBackendIDChan <- ourBackendID

	job, err := c.LockJob("")
	require.NoError(t, err)
	defer job.Done()

	deletedJobID := <-deletedJobIDChan

	require.Less(t, deletedJobID, job.ID)
}

func TestJobDelete(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)
	defer j.Done()

	err = j.Delete()
	require.NoError(t, err)

	// make sure job was deleted
	j2 := findOneJob(t, c.pool)
	assert.Nil(t, j2)
}

func TestJobDone(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)

	j.Done()

	// make sure conn and pool were cleared
	assert.Nil(t, j.conn)
	assert.Nil(t, j.pool)

	// make sure lock was released
	var count int64
	query := "SELECT count(*) FROM pg_locks WHERE locktype = $1 AND objid= $2::bigint"
	err = c.pool.QueryRow(query, "advisory", j.ID).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	// make sure conn was returned to pool
	stat := c.pool.Stat()
	total, available := stat.CurrentConnections, stat.AvailableConnections
	assert.Equal(t, available, total)
}

func TestJobDoneMultiple(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)

	j.Done()
	// try calling Done() again
	j.Done()
}

func TestJobDeleteFromTx(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)

	// get the job's database connection
	conn := j.Conn()
	require.NotNil(t, conn)

	// start a transaction
	tx, err := conn.Begin()
	require.NoError(t, err)

	// delete the job
	err = j.Delete()
	require.NoError(t, err)

	err = tx.Commit()
	require.NoError(t, err)

	// mark as done
	j.Done()

	// make sure the job is gone
	j2 := findOneJob(t, c.pool)
	assert.Nil(t, j2)
}

func TestJobDeleteFromTxRollback(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j1, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j1)

	// get the job's database connection
	conn := j1.Conn()
	require.NotNil(t, conn)

	// start a transaction
	tx, err := conn.Begin()
	require.NoError(t, err)

	// delete the job
	err = j1.Delete()
	require.NoError(t, err)

	err = tx.Rollback()
	require.NoError(t, err)

	// mark as done
	j1.Done()

	// make sure the job still exists and matches j1
	j2 := findOneJob(t, c.pool)
	require.NotNil(t, j2)

	assert.Equal(t, j1.ID, j2.ID)
}

func TestJobError(t *testing.T) {
	c := openTestClient(t)

	err := c.Enqueue(&Job{Type: "MyJob"})
	require.NoError(t, err)

	j, err := c.LockJob("")
	require.NoError(t, err)
	require.NotNil(t, j)
	defer j.Done()

	msg := "world\nended"
	err = j.Error(msg)
	require.NoError(t, err)
	j.Done()

	// make sure job was not deleted
	j2 := findOneJob(t, c.pool)
	require.NotNil(t, j2)
	defer j2.Done()

	assert.NotEqual(t, pgtype.Null, j2.LastError.Status)
	assert.Equal(t, msg, j2.LastError.String)
	assert.Equal(t, int32(1), j2.ErrorCount)

	// make sure lock was released
	var count int64
	query := "SELECT count(*) FROM pg_locks WHERE locktype=$1 AND objid=$2::bigint"
	err = c.pool.QueryRow(query, "advisory", j.ID).Scan(&count)
	require.NoError(t, err)

	assert.Equal(t, int64(0), count, "advisory lock was not released")

	// make sure conn was returned to pool
	stat := c.pool.Stat()
	total, available := stat.CurrentConnections, stat.AvailableConnections
	assert.Equal(t, available, total)
}
