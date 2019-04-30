package worker

/*

Theory of Operation

The worker process pulls jobs from the farmer and runs the job's tests.

The `testbot worker` command runs on EC2. It:

* long polls the `testbot farmer` service
* receives a job
* clones the job's `SHA`
* changes to the job's directory
* runs the commands in the job directory's `Testfile`
* reports results back to the `testbot farmer` service

*/

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	s3pkg "github.com/aws/aws-sdk-go/service/s3"
	"golang.org/x/xerrors"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"github.com/wepogo/testbot"
	"github.com/wepogo/testbot/log"
	"github.com/wepogo/testbot/trace"
)

// We have some jobs that actually take over 45s to run,
// so this is about as tight as we can make it right now.
const jobTimeout = 3 * time.Minute

func or(v, d string) string {
	if v == "" {
		v = d
	}
	return v
}

var (
	boxID       = randID()
	hostname, _ = os.Hostname()
	org         = or(os.Getenv("GITHUB_ORG"), "wepogo")
	repo        = or(os.Getenv("GITHUB_REPO"), "pogo")
	repoURL     = "https://github.com/" + org + "/" + repo + ".git"
	farmerURL   = or(os.Getenv("FARMER_URL"), "https://testbot.seqint.com")
	// httpClient is used for all http requests so that we amortize the setup costs
	httpClient = http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   20 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}}

	// If compiled with -tags aws, regionS3, bucket, netlify
	// and gitCredentials will be overwritten with the value
	// from Parameter Store.
	regionS3       = "us-west-1"
	gitCredentials = os.Getenv("GIT_CREDENTIALS")
	bucket         = os.Getenv("S3_BUCKET")
	netlify        = os.Getenv("NETLIFY_AUTH_TOKEN")

	// Directory layout
	rootDir = path.Join(os.Getenv("HOME"), "worker")
	binDir  = path.Join(os.Getenv("HOME"), "bin")
	outDir  = path.Join(rootDir, "out")
	wsDir   = path.Join(rootDir, "ws")
	repoDir = path.Join(wsDir, "src/"+or(os.Getenv("DIRNAME"), repo))

	pingReq = testbot.BoxPingReq{
		ID:   boxID,
		Host: hostname,
	}

	s3 *s3pkg.S3

	curMu  sync.Mutex
	curOut string
	curJob testbot.Job
)

// Main registers box with farmer, waits for jobs
func Main() {
	fmt.Println("starting box", boxID)

	if gitCredentials != "" {
		usr, err := user.Current()
		if err != nil {
			log.Fatalkv(context.Background(), log.Error, err, "at", "getting current user")
		}
		gitfile := usr.HomeDir + "/.git-credentials"

		// write credentials to ~/.git-credentials
		must(ioutil.WriteFile(gitfile, []byte(gitCredentials+"\n"), 0700))

		// update ~/.gitconfig to be configured to use ~/.git-credentials
		must(
			command(
				context.Background(),
				os.Stdout,
				"git",
				"config",
				"--global",
				"credential.helper",
				fmt.Sprintf("store --file %v", gitfile),
			).Run(),
		)
	}

	tracer.Start(tracer.WithSampler(tracer.NewAllSampler()))

	s3 = s3pkg.New(session.Must(session.NewSession(
		aws.NewConfig().WithRegion(regionS3),
	)))

	initFilesystem()

	ping()
	go func() {
		for {
			time.Sleep(time.Second)
			ping() // crash if this fails
		}
	}()
	go pollForOutput()

	state := testbot.BoxState{ID: boxID}
	cancel := func() {}
	for {
		state = waitState(state)
		cancel()
		cancel = startJob(state.Job)
	}
}

// OneJob is like main, but runs a single job
// without registering with the farmer.
// It writes output to stdout instead of S3.
// It requires all the same environment as Main.
func OneJob(job testbot.Job) {
	initFilesystem()
	ctx := context.Background()
	cmd, _, err := startJobProc(ctx, os.Stdout, job)
	if err != nil {
		fmt.Fprintln(os.Stderr, job, err)
		os.Exit(2)
	}
	err = cmd.Wait()
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // kill entire process group
	if err != nil {
		fmt.Fprintln(os.Stderr, job, err)
		os.Exit(2)
	}
}

func ping() {
	err := postJSON("/box-ping", pingReq, nil)
	if err != nil {
		log.Fatalkv(
			context.Background(),
			"error",
			"farmer not available. check FARMER_URL. "+err.Error(),
		)
	}
}

func pollForOutput() {
	ctx := context.Background()
	for {
		var job testbot.Job
		err := postJSON("/box-livepoll", struct{ ID string }{boxID}, &job)
		if err != nil {
			log.Error(ctx, err)
			// Normally this is a long poll, so it's good
			// to reconnect immediately. But if there was
			// an error, it could have happened quickly,
			// so avoid hammering the server.
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if job == (testbot.Job{}) {
			continue
		}
		go sendOutput(job)

		// Give our sendOutput RPC a chance to consume
		// the request for job output before we poll again.
		// If we poll immediately, we are more likely
		// to pick up the same request again.
		// It's not so bad if that happens sometimes
		// (all but one sendOutput body will be dropped),
		// it's just a little wasteful. So avoid it.
		time.Sleep(50 * time.Millisecond)
	}
}

func initFilesystem() {
	ctx := context.Background()
	must(os.RemoveAll(rootDir))
	must(os.MkdirAll(wsDir, 0700))
	must(os.MkdirAll(outDir, 0700))
	must(command(ctx, os.Stdout, "git", "clone", repoURL, repoDir).Run())
	must(runIn(ctx, repoDir, command(ctx, os.Stdout, "git", "checkout", "-bt")))
}

func waitState(oldState testbot.BoxState) (newState testbot.BoxState) {
	newState = getState(oldState)
	for newState == oldState {
		newState = getState(oldState)
	}
	return newState
}

func getState(oldState testbot.BoxState) testbot.BoxState {
	var newState testbot.BoxState
	err := postJSON("/box-longpoll", oldState, &newState)
	if err != nil {
		time.Sleep(time.Second)
		return oldState
	}
	return newState
}

func startJob(job testbot.Job) func() {
	start := time.Now()
	if job == (testbot.Job{}) {
		// nothing to do
		return func() {}
	}

	// TODO(kr): connect to trace from farmer
	span := tracer.StartSpan("job", tracer.ServiceName("testbot-worker"), tracer.ResourceName(job.Dir+" "+job.Name))
	span.SetTag("job.sha", job.SHA)
	span.SetTag("job.dir", job.Dir)
	span.SetTag("job.name", job.Name)

	jobCtx := context.Background()
	jobCtx = tracer.ContextWithSpan(jobCtx, span)

	postStatus := func(status, desc, url string) {
		req := testbot.BoxJobUpdateReq{
			Job:    job,
			Status: status,
			Desc:   desc,
			URL:    url,
		}
		if status != "pending" {
			defer span.Finish()
			req.Elapsed = time.Since(start)
			req.TraceURL = traceURL(span)
		}
		postJSON("/box-runstatus", req, nil)
	}

	postStatus("pending", "running", "")

	f, err := os.Create(path.Join(outDir, outputFile(job)))
	if err != nil {
		fmt.Fprintln(os.Stderr, job, err)
		postStatus("error", err.Error(), "")
		return func() {}
	}

	curMu.Lock()
	curOut = f.Name()
	curJob = job
	curMu.Unlock()

	cmddir := filepath.Join(repoDir, filepath.FromSlash(job.Dir))

	// must be called exactly once (to close f)
	uploadAndPostStatus := func(status, desc string) {
		defer func() {
			curMu.Lock()
			curJob = testbot.Job{}
			curOut = ""
			curMu.Unlock()
		}()
		defer f.Close()

		fmt.Fprintln(f, desc)
		f.Seek(0, 0)
		if s := scanError(f); s != "" && status != "success" {
			s = strings.Replace(s, cmddir+"/", "", -1)
			s = strings.Replace(s, repoDir+"/", "$I10R/", -1)
			desc += ": " + s
		}
		f.Seek(0, 0)
		u, err := uploadToS3(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, job, "cannot upload output file", err)
			postStatus("error", "S3 upload: "+err.Error(), "")
			return
		}
		postStatus(status, desc, u)
	}

	jobCtx, cancel := context.WithTimeout(jobCtx, jobTimeout)
	cmd, cmdSpan, err := startJobProc(jobCtx, f, job)
	if err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, job, err)
		uploadAndPostStatus("error", err.Error())
		return func() {}
	}

	// wait for job, post result status
	done := make(chan int)
	go func() {
		defer close(done) // ok to start next job

		jobErr := cmd.Wait()
		cmdSpan.Finish()
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // kill entire process group

		if jobErr != nil && jobCtx.Err() != nil {
			uploadAndPostStatus("error", fmt.Sprintf("canceled automatically: %s: %s", jobCtx.Err(), jobErr))
		} else if jobErr != nil {
			fmt.Fprintln(os.Stderr, job, "failure running job", jobErr)
			uploadAndPostStatus("failure", jobErr.Error())
		} else {
			fmt.Fprintln(os.Stderr, job, "success running job")
			ms := time.Since(start) / time.Millisecond
			uploadAndPostStatus("success", fmt.Sprintf("%dms", ms))
		}
	}()

	return func() { cancel(); <-done }
}

func startJobProc(ctx context.Context, w io.Writer, job testbot.Job) (*exec.Cmd, tracer.Span, error) {
	fmt.Fprintln(w, "starting job", job)
	fmt.Fprintln(w, "worker host", hostname)

	start := time.Now()
	var setupBuf bytes.Buffer
	err := setupJob(ctx, &setupBuf, job.SHA)
	if err != nil {
		w.Write(setupBuf.Bytes())
		return nil, nil, xerrors.Errorf("clone: %w", err)
	}
	fmt.Fprintln(w, "setup ok", time.Since(start))
	cmddir := path.Join(repoDir, job.Dir)

	// Before we run actual tests, traverse the tree to find all `setup` tasks in all Testfiles
	// and run these tasks first. This will guarantee, for example, that when a Go package depends
	// on a Rust crate, that crate will be built before the tests or `go vet` would run.
	//
	// WARNING: We do not guarantee any particular order. If you have cross-directory
	// dependencies you should configure your own Makefiles.

	// Traverse all folders, open testfiles, read `setup` tasks and run them.
	err = filepath.Walk(cmddir, func(fullPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Name() != "Testfile" {
			return nil
		}

		testfile, err := os.Open(fullPath)
		if err != nil {
			return err
		}
		defer testfile.Close()

		entries, err := testbot.ParseTestfile(testfile)
		if err != nil {
			return err
		}

		// Note: the "setup" key has a special meaning and is therefore ignored by the tests.
		cmd := entries["setup"]

		if cmd == "" {
			return nil
		}

		c := prepareCommand(ctx, filepath.Dir(fullPath), w, cmd)
		return c.Run()
	})

	if err != nil {
		return nil, nil, err
	}

	// Run the actual tests:

	testfile, err := os.Open(path.Join(cmddir, "Testfile"))
	if err != nil {
		return nil, nil, err
	}
	defer testfile.Close()

	entries, err := testbot.ParseTestfile(testfile)
	if err != nil {
		fmt.Fprintf(w, "parse %s: %v\n", testfile.Name(), err)
		return nil, nil, err
	}

	cmd, ok := entries[job.Name]
	if !ok {
		fmt.Fprintln(w, "cannot find Testfile entry", job.Name)
		return nil, nil, xerrors.Errorf("cannot find Testfile entry %s", job.Name)
	}

	span, ctx := tracer.StartSpanFromContext(ctx, "runtest")
	span.SetTag("cmd", cmd)
	span.SetTag("dir", cmddir)
	// no span.Finish() call here, that happens in startJob

	c := prepareCommand(ctx, cmddir, w, cmd)
	return c, span, c.Start()
}

func prepareCommand(ctx context.Context, dir string, w io.Writer, cmd string) *exec.Cmd {
	c := command(ctx, w, "/bin/bash", "-eo", "pipefail", "-c", cmd)
	c.Env = append(os.Environ(),
		"CHAIN="+repoDir,
		"I10R="+repoDir,
		"GOBIN="+binDir,
		"NETLIFY_AUTH_TOKEN="+netlify,
		"PATH="+binDir+":"+repoDir+"/bin:"+os.Getenv("PATH"),
	)
	c.Env = append(c.Env, trace.EnvironmentFor(ctx)...)
	c.Dir = dir
	fmt.Fprintln(w, "cd", c.Dir)
	fmt.Fprintln(w, cmd)
	return c
}

func sendOutput(j testbot.Job) {
	ctx := context.Background()
	f, err := getOutput(j)
	if err != nil {
		log.Error(ctx, err)
		return
	}
	defer f.Close()
	body := &follower{f: f}
	req, err := http.NewRequest("POST", farmerURL+"/box-livesend", body)
	if err != nil {
		log.Error(ctx, err)
		return
	}
	req.Header.Set("Box-ID", boxID)
	req.Header.Set("Job-SHA", j.SHA)
	req.Header.Set("Job-Dir", j.Dir)
	req.Header.Set("Job-Name", j.Name)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error(ctx, err)
		return
	}
	resp.Body.Close()
}

func getOutput(j testbot.Job) (*os.File, error) {
	curMu.Lock()
	if curJob != j {
		curMu.Unlock()
		return nil, xerrors.New("not found")
	}
	name := curOut
	curMu.Unlock()

	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func outputFile(job testbot.Job) string {
	// use the trick from RFC 6901 (JSON Pointer)
	// to encode "/" in a single path component.
	dir := job.Dir
	dir = strings.Replace(dir, "~", "~0", -1)
	dir = strings.Replace(dir, "/", "~1", -1)
	return job.SHA + "-" + dir + "-" + job.Name + "." + randID() + ".output"
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func setupJob(ctx context.Context, w io.Writer, sha string) error {
	span, ctx := tracer.StartSpanFromContext(ctx, "setup")
	defer span.Finish()

	// Make sure we have sha in the local clone.
	if !objectExists(ctx, w, sha) {
		err := runIn(ctx, repoDir, command(ctx, w, "git", "fetch"))
		if err != nil {
			// Sometimes this fails, and trying again usually works.
			// So try again just one more time, after a brief wait.
			// If it still fails after that, give up.
			time.Sleep(2 * time.Second)
			err = runIn(ctx, repoDir, command(ctx, w, "git", "fetch"))
		}
		if err != nil {
			return err
		}
	}

	err := runIn(ctx, repoDir, command(ctx, w, "git", "clean", "-xdf"))
	if err != nil {
		return err
	}
	return runIn(ctx, repoDir, command(ctx, w, "git", "reset", "--hard", sha))
}

// objectExists returns whether the object definitely exists.
// It returns false if the object doesn't exist, or if there
// was an error.
func objectExists(ctx context.Context, w io.Writer, sha string) bool {
	err := runIn(ctx, repoDir, command(ctx, w, "git", "cat-file", "-e", sha))
	return err == nil
}

func runIn(ctx context.Context, dir string, c *exec.Cmd) error {
	span, _ := tracer.StartSpanFromContext(ctx, "run")
	defer span.Finish()

	c.Dir = dir
	logCmd(c)
	span.SetTag("path", c.Path)
	span.SetTag("args", fmt.Sprintf("%q", c.Args))
	span.SetTag("dir", c.Dir)

	return c.Run()
}

func command(ctx context.Context, w io.Writer, name string, arg ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, arg...)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Stdout = w
	c.Stderr = w
	return c
}

func logCmd(cmd *exec.Cmd) {
	if cmd.Dir != "" {
		fmt.Fprintln(cmd.Stdout, "cd", cmd.Dir)
	}
	fmt.Fprintln(cmd.Stdout, strings.Join(cmd.Args, " "))
}

// scanError scans through r until it finds a line
// that looks like a compiler error message
//   path/to/file.ext:123: any text here
// It returns the first such line it encounters, if any.
func scanError(r io.Reader) string {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if looksLikeError(line) {
			return line
		}
	}
	return ""
}

func looksLikeError(line string) bool {
	// TypeScript style (tsc, tslint, etc)
	if strings.HasPrefix(line, "ERROR: ") {
		return true
	}

	// Traditional style (gcc, go, etc)
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return false
	}
	file, rest := line[:i], line[i+1:]
	i = strings.IndexByte(rest, ':')
	if i < 0 || strings.IndexByte(file, ' ') >= 0 {
		return false
	}
	_, err := strconv.Atoi(rest[:i])
	return err == nil && !strings.Contains(rest[i:], "warning:")
}

func randID() string {
	b := make([]byte, 10)
	_, err := rand.Read(b)
	if err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// A follower acts like 'tail -f'.
// It reads from f to the end, then waits for more data
// to be appended to f, and it reads that too.
// It returns EOF when curOut and f are no longer
// the same file (while f is at the end).
type follower struct {
	f *os.File
	n int64
}

func (f *follower) Read(p []byte) (int, error) {
	for {
		running := isCur(f.f)
		n, err := f.f.Read(p)
		f.n += int64(n)
		if err != nil && err != io.EOF {
			return n, err
		}
		if n == 0 && err == io.EOF && !running {
			return n, io.EOF
		}
		if n == 0 {
			time.Sleep(100 * time.Millisecond)
			continue // nothing happened, try again
		}
		return n, nil
	}
}

func isCur(f *os.File) bool {
	curMu.Lock()
	defer curMu.Unlock()
	return curOut == f.Name()
}

func traceURL(span tracer.Span) string {
	const f = "https://app.datadoghq.com/apm/trace/%d?spanID=%d"
	return fmt.Sprintf(f, span.Context().TraceID(), span.Context().SpanID())
}