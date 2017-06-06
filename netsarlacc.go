package main

import (
        "flag"
        "fmt"
        "os"
	"os/signal"
	"syscall"
	"time"
        "net"
	"path/filepath"
	"errors"
)

//TODO:
// -- Determine a payload struct for headers
// -- ALL CAPS letters for headers up to 20 bytes up to space
// -- URL encoding then space
// -- http 1.1 etc
// -- encode whole raw packet up to 4kb in one of the fileds in JSON whether it was valid or not
// -- Stress testing APACHE for golang
// -- Test cases
// -- Build Template for response
// -- Test speed and race conditions
// -- Test with real connections
// -- Format for writing to files

const (
        // Set these to flags
        CONN_HOST = "0.0.0.0"
        CONN_PORT = "8888"
        CONN_TYPE = "tcp"
)

var (
        sinkHost, _      = os.Hostname()
        NWorkers         = flag.Int("n", 4, "The number of workers to start")
        SinkholeInstance = flag.String("i", "netsarlacc-" + sinkHost, "The sinkhole instance name")
	Daemonize        = flag.Bool("D", false, "Daemonize the sinkhole")
        Logchan = make(chan string, 1024)
	Stopchan = make(chan os.Signal, 1)
	Workerstopchan = make(chan bool, 1)
	Logstopchan = make(chan bool, 1)
	Daemonized = false
	PidFile *os.File
)

func main() {

	// Setup the stop channel signal handler
	signal.Notify(Stopchan, os.Interrupt, syscall.SIGTERM)

        // Parse the command-line flags.
        flag.Parse()

	// Check if we should daemonize
	if *Daemonize == true {
		pid, err := DaemonizeProc()

		if err != nil {
			AppLogger(errors.New(fmt.Sprintf("Daemonization failed: %s", err.Error())))
			FatalAbort(false, -1)
		}

		if pid != nil {
			// This means we started a proc and we're not the daemon
			os.Exit(0)
		}
	}

        //starts the dispatcher
        StartDispatcher(*NWorkers)
        //starts the log channel
        go writeLogger(Logchan)

	// Get the TCPAddr
	listenAddr, err := net.ResolveTCPAddr(CONN_TYPE, CONN_HOST + ":" + CONN_PORT)
	if err != nil {
                AppLogger(errors.New(fmt.Sprintf("Unable to resolve listening address: %s", err.Error())))
		FatalAbort(false, -1)
        }

        //listen for incoming connections
        listen, err := net.ListenTCP(CONN_TYPE, listenAddr)
        if err != nil {
                AppLogger(errors.New(fmt.Sprintf("Error listening: %s", err.Error())))
		FatalAbort(false, -1)
        }

	AppLogger(errors.New("Listening on " + CONN_TYPE + " " + CONN_HOST + ":" + CONN_PORT))
	// Loop until we get the stop signal
	running := true
	for (running == true) {
		// Set a listen timeout so we don't block indefinitely
		listen.SetDeadline(time.Now().Add(time.Second))

		// Listen for any incoming connections or possibly time out
		connection, err := listen.Accept()

		// Check to see if we're supposed to stop
		select {
		case <-Stopchan:
			running = false
			AppLogger(errors.New("Got signal to stop..."))
			// We'll let the connection we just accepted / timed out on get handled still
		default:
			// The Stopchan would have blocked because it's empty
		}

		if err != nil {

			netErr, ok := err.(net.Error)
			// If this was a timeout just keep going
			if ((ok == true) && (netErr.Timeout() == true) && (netErr.Temporary() == true)) {
				continue;
			} else {
				AppLogger(errors.New(fmt.Sprintf("Error accepting: %s", err.Error())))
			}
		} else {
			go Collector(connection)
		}
	} // End while running

	// We must be stopping, close the listening socket
	AppLogger(errors.New("Shutting down listening socket"))
	listen.Close()

	// Shut the rest of everything down
	AttemptShutdown()
}


func AttemptShutdown() {

	// The goal here is to try to stop the workers
	// and close out the log file so that data isn't lost
	// and the log file is left consistent

	AppLogger(errors.New("Stopping workers"))
	StopWorkers()

	// As workers stop, they will tell us that.  We must wait for them
	// so that we can close the logging after the pending work is done
	for wstopped := 0; wstopped < *NWorkers; {
		select {
		case <-Workerstopchan:
			wstopped++
		case <-time.After(time.Second * 5):
			AppLogger(errors.New("Timed out waiting for all workers to stop!"))
			wstopped = *NWorkers
		}
	}

	// Close the Logchan which will allow the remaining
	// logs to get written to the logfile before the logging
	// goroutine finally closes the file and tells us it finished
	AppLogger(errors.New("Flushing logs and closing the log file"))
	close(Logchan)

	select {
	case <-Logstopchan:
		break
	case <-time.After(time.Second * 5):
		AppLogger(errors.New("Timed out waiting for log flushing and closing!"))
		break
	}

	if Daemonized == true {
		AppLogger(errors.New("Releasing lock on PID file"))
		err := syscall.Flock(int(PidFile.Fd()), syscall.LOCK_UN)

		if err != nil {
			AppLogger(errors.New(fmt.Sprintf("Unable to release lock on pid file: %s", err.Error())))
			FatalAbort(false, -1)
		}

		AppLogger(errors.New("Closing out PID file"))
		err = PidFile.Close()

		if err != nil {
			AppLogger(errors.New(fmt.Sprintf("Unable to close pid file: %s", err.Error())))
			FatalAbort(false, -1)
		}
	}

	AppLogger(errors.New("Shutdown complete"))
}


func FatalAbort(cleanup bool, ecode int) {

	// If we're supposed to cleanup we'll attempt that first
	if cleanup == true {
		AttemptShutdown()
	}

	AppLogger(errors.New(fmt.Sprintf("Aborting with error code %d", ecode)))
	os.Exit(ecode)
}


func Fullpath(exe string) (string, error) {

	// Bail out if the exe string is empty
	if len(exe) == 0 {
		return "", os.ErrInvalid
	}

	// Get an absolute path
	fullpath, err := filepath.Abs(exe);

	if err != nil {
		return "", err
	}

	// Optionally we could also call EvalSymlinks to get the real path
	// but I don't think that's needed here

	return fullpath, nil
}


func DaemonizeProc() (*os.Process, error) {
	pidlockchan := make(chan error, 1)

	_, daemonVarExists := os.LookupEnv("_NETSARLACC_DAEMON")

	if daemonVarExists == true {
		// We're the started daemon
		Daemonized = true

		// Unset the env var
		err := os.Unsetenv("_NETSARLACC_DAEMON")

		if err != nil {
			return nil, err
		}

		// Break away from the parent
		pid, err := syscall.Setsid()

		if err != nil {
			return nil, err
		}

		// We may want to indicate that we need to setuid/gid here but
		// we'll have to figure out how to Setuid after the socket are bound

		// We may want to ensure we're at / by seting our cwd there too

		// Report the PID that we got
		AppLogger(errors.New(fmt.Sprintf("Daemon got a PID of %d", pid)))

		// Now open our PID file, get a lock, and write our PID to it
		PidFile, err = os.OpenFile("netsarlacc.pid", os.O_TRUNC|os.O_WRONLY|os.O_CREATE, 0644)

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to open pid file: %s", err.Error()))
			return nil, err
		}

		// Now try to get an exclusive lock the file descriptor
		go func() {
			err := syscall.Flock(int(PidFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

			if err != nil {
				err = errors.New(fmt.Sprintf("Unable to acquire lock on pid file: %s", err.Error()))
			}

			pidlockchan <- err
		}()

		var lockerr error
		select {
		case lockerr =  <- pidlockchan:
			if lockerr != nil {
				return nil, lockerr
			}
			break
		case <-time.After(time.Second * 5):
			err = errors.New("Timed out waiting for pid lock acquisition!")
			return nil, err
		}

		// Write our PID to the file
		_, err = PidFile.Write([]byte(fmt.Sprintf("%d\n", pid)))

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to write pid into pid file: %s", err.Error()))
			return nil, err
		}

		// Make sure the PID actually gets to the file because we aren't
		// going to close the file until the daemon is about to exit
		// so that we can keep holding onto our lock of the file
		err = PidFile.Sync()

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to sync written pid into pid file: %s", err.Error()))
			return nil, err
		}

		return nil, nil
	} else {
		// We need to start the daemon
		AppLogger(errors.New(fmt.Sprintf("Daemonizing...")))

		var err error
		// First we'll try to open and acquire a lock on the pid file
		// to ensure there isn't a daemon already running
		PidFile, err = os.OpenFile("netsarlacc.pid", os.O_RDONLY|os.O_CREATE, 0644)

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to open pid file: %s", err.Error()))
			return nil, err
		}

		// Now try to get an exclusive lock the file descriptor
		go func() {
			err := syscall.Flock(int(PidFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)

			if err != nil {
				err = errors.New(fmt.Sprintf("Unable to acquire lock on pid file: %s", err.Error()))
			}

			pidlockchan <- err
		}()

		var lockerr error
		select {
		case lockerr =  <- pidlockchan:
			if lockerr != nil {
				return nil, lockerr
			}
			break
		case <-time.After(time.Second * 5):
			err = errors.New("Timed out waiting for pid lock acquisition!")
			return nil, err
		}

		// The lock worked so let's release it and then close the file
		err = syscall.Flock(int(PidFile.Fd()), syscall.LOCK_UN)

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to release lock on pid file: %s", err.Error()))
			return nil, err
		}

		err = PidFile.Close()

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to close pid file: %s", err.Error()))
			return nil, err
		}

		// Get our exename and full path
		exe, err := Fullpath(os.Args[0])

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to get full path of exe: %s", err.Error()))
			return nil, err
		}

		var attrs os.ProcAttr
		// Eventually we may want to set the new proc's dir to /
		// but this requires that we have support for a logging directory
		// instead of just using the cwd for logs
		// attrs.Dir = "/"

		// Set the new process's stdin, stdout, and stderr to /dev/null
		f_devnull, err := os.Open("/dev/null")

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to open /dev/null: %s", err.Error()))
			return nil, err
		}
		attrs.Files = []*os.File{f_devnull, f_devnull, f_devnull}

		// Tell the next process it's the deamon
		os.Setenv("_NETSARLACC_DAEMON", "true")

		// Try to start up the deamon process
		pid, err := os.StartProcess(exe, os.Args, &attrs)

		if err != nil {
			err = errors.New(fmt.Sprintf("Unable to start daemon process: %s", err.Error()))
			return nil, err
		}

		fmt.Fprintln(os.Stderr, fmt.Sprintf("Daemon started as PID %d", pid.Pid))

		return pid, nil
	}
}
