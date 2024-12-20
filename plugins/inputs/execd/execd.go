//go:generate ../../../tools/readme_config_includer/generator
package execd

import (
	"bufio"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/internal/process"
	"github.com/influxdata/telegraf/models"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/parsers/influx"
)

//go:embed sample.conf
var sampleConfig string

var once sync.Once

type Execd struct {
	Command      []string        `toml:"command"`
	Environment  []string        `toml:"environment"`
	BufferSize   config.Size     `toml:"buffer_size"`
	Signal       string          `toml:"signal"`
	RestartDelay config.Duration `toml:"restart_delay"`
	StopOnError  bool            `toml:"stop_on_error"`
	Log          telegraf.Logger `toml:"-"`

	process      *process.Process
	acc          telegraf.Accumulator
	parser       telegraf.Parser
	outputReader func(io.Reader)
}

func (*Execd) SampleConfig() string {
	return sampleConfig
}

func (e *Execd) Init() error {
	if len(e.Command) == 0 {
		return errors.New("no command specified")
	}
	return nil
}

func (e *Execd) SetParser(parser telegraf.Parser) {
	e.parser = parser
	e.outputReader = e.cmdReadOut

	unwrapped, ok := parser.(*models.RunningParser)
	if ok {
		if _, ok := unwrapped.Parser.(*influx.Parser); ok {
			e.outputReader = e.cmdReadOutStream
		}
	}
}

func (e *Execd) Start(acc telegraf.Accumulator) error {
	e.acc = acc
	var err error
	e.process, err = process.New(e.Command, e.Environment)
	if err != nil {
		return fmt.Errorf("error creating new process: %w", err)
	}
	e.process.ReadStdoutFn = e.outputReader
	e.process.ReadStderrFn = e.cmdReadErr
	e.process.RestartDelay = time.Duration(e.RestartDelay)
	e.process.StopOnError = e.StopOnError
	e.process.Log = e.Log

	if err = e.process.Start(); err != nil {
		// if there was only one argument, and it contained spaces, warn the user
		// that they may have configured it wrong.
		if len(e.Command) == 1 && strings.Contains(e.Command[0], " ") {
			e.Log.Warn("The inputs.execd Command contained spaces but no arguments. " +
				"This setting expects the program and arguments as an array of strings, " +
				"not as a space-delimited string. See the plugin readme for an example.")
		}
		return fmt.Errorf("failed to start process %s: %w", e.Command, err)
	}

	return nil
}

func (e *Execd) Stop() {
	e.process.Stop()
}

func (e *Execd) cmdReadOut(out io.Reader) {
	rdr := bufio.NewReaderSize(out, int(e.BufferSize))

	for {
		data, err := rdr.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				break
			}
			e.acc.AddError(fmt.Errorf("error reading stdout: %w", err))
			continue
		}

		metrics, err := e.parser.Parse(data)
		if err != nil {
			e.acc.AddError(fmt.Errorf("parse error: %w", err))
		}

		if len(metrics) == 0 {
			once.Do(func() {
				e.Log.Debug(internal.NoMetricsCreatedMsg)
			})
		}

		for _, metric := range metrics {
			e.acc.AddMetric(metric)
		}
	}
}

func (e *Execd) cmdReadOutStream(out io.Reader) {
	parser := influx.NewStreamParser(out)

	for {
		metric, err := parser.Next()
		if err != nil {
			if errors.Is(err, influx.EOF) {
				break // stream ended
			}
			var parseErr *influx.ParseError
			if errors.As(err, &parseErr) {
				// parse error.
				e.acc.AddError(parseErr)
				continue
			}
			// some non-recoverable error?
			e.acc.AddError(err)
			return
		}

		e.acc.AddMetric(metric)
	}
}

func (e *Execd) cmdReadErr(out io.Reader) {
	scanner := bufio.NewScanner(out)

	for scanner.Scan() {
		msg := scanner.Text()
		switch {
		case strings.HasPrefix(msg, "E! "):
			e.Log.Error(msg[3:])
		case strings.HasPrefix(msg, "W! "):
			e.Log.Warn(msg[3:])
		case strings.HasPrefix(msg, "I! "):
			e.Log.Info(msg[3:])
		case strings.HasPrefix(msg, "D! "):
			e.Log.Debug(msg[3:])
		case strings.HasPrefix(msg, "T! "):
			e.Log.Trace(msg[3:])
		default:
			e.Log.Errorf("stderr: %q", msg)
		}
	}

	if err := scanner.Err(); err != nil {
		e.acc.AddError(fmt.Errorf("error reading stderr: %w", err))
	}
}

func init() {
	inputs.Add("execd", func() telegraf.Input {
		return &Execd{
			Signal:       "none",
			RestartDelay: config.Duration(10 * time.Second),
			BufferSize:   config.Size(64 * 1024),
		}
	})
}
