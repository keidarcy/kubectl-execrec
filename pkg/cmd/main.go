package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

const version = "v1.0.0"

type ExecRec struct {
	// io
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer

	// args to forward to kubectl exec
	args []string

	// runtime state
	// logDir is the directory to store the log file
	logDir string
	// logPath is the path to the log file
	logPath string
	// logFile is the log file
	logFile *os.File
	// username is the username of the user running the command
	username string

	// cmd is the kubectl exec command
	cmd *exec.Cmd
	// ptyFile is the PTY file
	ptyFile *os.File
	// restoreTTY restores the terminal to its original state
	restoreTTY func() error
	// stopSigs stops the signal handlers
	stopSigs func()
}

// NewCmd creates a new cobra command
func NewCmd(streams genericclioptions.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "execrec [kubectl exec args...]",
		Version: version,
		Short:   "Wrapper around 'kubectl exec' with session logging",
		Long: `kubectl execrec is a wrapper around 'kubectl exec' that logs all session output to a file.

This command forwards all arguments to 'kubectl exec' and captures the session for logging purposes.

Examples:
  kubectl execrec -n namespace pod-name -it -- bash
  kubectl execrec -n default my-pod -- ls -la
  KUBECTL_EXECREC_S3_BUCKET=my-bucket kubectl execrec -n kube-system pod-name -it -- sh
  KUBECTL_EXECREC_S3_ENDPOINT=https://my-endpoint.com KUBECTL_EXECREC_S3_BUCKET=my-bucket kubectl execrec -n kube-system pod-name -it -- sh`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// Check os.TempDir()/kubectl-execrec exists
			logDir := filepath.Join(os.TempDir(), "kubectl-execrec")
			if _, err := os.Stat(logDir); os.IsNotExist(err) {
				err = os.MkdirAll(logDir, 0755)
				if err != nil {
					return fmt.Errorf("failed to create log directory: %w", err)
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			rec := &ExecRec{
				stdin:    streams.In,
				stdout:   streams.Out,
				stderr:   streams.ErrOut,
				args:     args,
				username: whoami(),
				logDir:   filepath.Join(os.TempDir(), "kubectl-execrec"),
			}
			if err := rec.Prepare(); err != nil {
				return err
			}
			defer rec.CloseLog()

			if err := rec.Start(); err != nil {
				return err
			}

			rec.Stream()

			cmdErr := rec.cmd.Wait()

			// Clean up TTY before writing final messages
			rec.CleanupTTY()

			// Always finish the session to ensure log file is properly closed
			finishErr := rec.Finish()

			// Handle command error first
			if cmdErr != nil {
				return rec.Propagate(cmdErr)
			}

			// Then handle finish error
			return rec.Propagate(finishErr)
		},
	}

	cmd.DisableFlagParsing = true
	return cmd
}

// Prepare log file and write header
func (r *ExecRec) Prepare() error {
	if _, err := os.Stat(r.logDir); os.IsNotExist(err) {
		if err := os.MkdirAll(r.logDir, 0o755); err != nil {
			return fmt.Errorf("failed to create log directory: %w", err)
		}
	}

	timestamp := time.Now().Format(time.RFC3339)
	logFileName := fmt.Sprintf("%s_%s.log", r.username, timestamp)
	r.logPath = filepath.Join(r.logDir, logFileName)

	f, err := os.Create(r.logPath)
	if err != nil {
		return fmt.Errorf("failed to create log file: %w", err)
	}
	r.logFile = f

	// header
	command := fmt.Sprintf("kubectl execrec %s", strings.Join(r.args, " "))
	session := fmt.Sprintf("start=%s user=%s version=%s", timestamp, r.username, version)
	_, err = r.logFile.WriteString(fmt.Sprintf("[command] %s\n[session] %s\n%s\n", command, session, strings.Repeat("=", 80)))
	if err != nil {
		return err
	}
	return r.logFile.Sync()
}

// Close log file
func (r *ExecRec) CloseLog() {
	if r.logFile != nil {
		r.logFile.Close()
	}
}

// Start PTY and inherit terminal size
func (r *ExecRec) Start() error {
	// build kubectl exec
	kargs := append([]string{"exec"}, r.args...)
	r.cmd = exec.Command("kubectl", kargs...)

	// start PTY
	ptmx, err := pty.Start(r.cmd)
	if err != nil {
		return fmt.Errorf("failed to start PTY: %w", err)
	}
	r.ptyFile = ptmx

	// inherit terminal size
	if err := pty.InheritSize(os.Stdin, r.ptyFile); err != nil {
		return fmt.Errorf("failed to inherit terminal size: %w", err)
	}

	// raw mode to keep tab works as before
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to put terminal in raw mode: %w", err)
	}
	r.restoreTTY = func() error { return term.Restore(int(os.Stdin.Fd()), oldState) }

	// forward SIGINT/SIGTERM to kubectl
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-sigChan:
				if r.cmd != nil && r.cmd.Process != nil {
					_ = r.cmd.Process.Signal(syscall.SIGTERM)
				}
			case <-stop:
				return
			}
		}
	}()
	r.stopSigs = func() {
		close(stop)
		signal.Stop(sigChan)
	}
	return nil
}

// Cleanup TTY before writing final messages
func (r *ExecRec) CleanupTTY() {
	if r.stopSigs != nil {
		r.stopSigs()
	}

	if r.restoreTTY != nil {
		_ = r.restoreTTY()
	}

	if r.ptyFile != nil {
		_ = r.ptyFile.Close()
	}
}

// Stream stdout and stderr to terminal and log file
func (r *ExecRec) Stream() {
	// PTY => (stdout + log)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.ptyFile.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				_, _ = r.stdout.Write(buf[:n])
				_, _ = r.logFile.Write(buf[:n])
				_ = r.logFile.Sync()
			}
		}
	}()

	// stdin => PTY
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.stdin.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				_, _ = r.ptyFile.Write(buf[:n])
			}
		}
	}()
}

// write footer and upload log file to S3 if KUBECTL_EXECREC_S3_BUCKET is set
func (r *ExecRec) Finish() error {
	// footer
	end := fmt.Sprintf("end=%s", time.Now().Format(time.RFC3339))
	_, err := r.logFile.WriteString(strings.Repeat("=", 80) + "\n")
	if err != nil {
		return err
	}
	_, err = r.logFile.WriteString(fmt.Sprintf("[session] %s\n", end))
	if err != nil {
		return err
	}
	err = r.logFile.Sync()
	if err != nil {
		return err
	}

	if os.Getenv("KUBECTL_EXECREC_S3_BUCKET") != "" {
		r.HandleS3Upload()
	} else {
		fmt.Fprintf(r.stdout, "Session logged to: %s\n", r.logPath)
	}
	return nil
}

// Upload log file to S3 if KUBECTL_EXECREC_S3_BUCKET environment variable is set
func (r *ExecRec) HandleS3Upload() {
	// check aws cli is installed
	if _, err := exec.LookPath("aws"); err != nil {
		fmt.Fprintf(r.stderr, "aws cli is not installed\n")
		return
	}

	s3Bucket := os.Getenv("KUBECTL_EXECREC_S3_BUCKET")

	s3Key := fmt.Sprintf("logs/%s", filepath.Base(r.logPath))
	s3Args := []string{"s3", "cp", r.logPath, fmt.Sprintf("s3://%s/%s", s3Bucket, s3Key)}
	if s3Endpoint := os.Getenv("KUBECTL_EXECREC_S3_ENDPOINT"); s3Endpoint != "" {
		s3Args = append([]string{"--endpoint-url", s3Endpoint}, s3Args...)
	}

	// Capture stderr to see what the error is
	var stderr bytes.Buffer
	uploadCmd := exec.Command("aws", s3Args...)
	uploadCmd.Stdout = nil
	uploadCmd.Stderr = &stderr

	if uploadErr := uploadCmd.Run(); uploadErr == nil {
		fmt.Fprintf(r.stdout, "\nLog file uploaded to s3://%s/%s\n", s3Bucket, s3Key)
	} else {
		fmt.Fprintf(r.stderr, "\nFailed to upload log file to s3://%s/%s\n", s3Bucket, s3Key)
		if stderr.Len() > 0 {
			fmt.Fprintf(r.stderr, "AWS CLI error: %s\n", stderr.String())
		}
		fmt.Fprintf(r.stdout, "Session logged to: %s\n", r.logPath)
	}
}

// Handle graceful termination (Ctrl+C, Ctrl+D, etc.)
func (r *ExecRec) Propagate(err error) error {
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			// expected codes: 130 (SIGINT), 143 (SIGTERM), 0
			code := ee.ExitCode()
			if code == 130 || code == 143 || code == 0 {
				return nil
			}
			os.Exit(code)
		}
		return err
	}
	return nil
}

// =========================== helpers ===========================
func whoami() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if v := os.Getenv("USER"); v != "" {
		return v
	}
	return "unknown"
}
