package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sync"

	"github.com/hashicorp/terraform/plugin"
	"github.com/mitchellh/cli"
	"github.com/mitchellh/panicwrap"
	"github.com/mitchellh/prefixedio"
)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	var wrapConfig panicwrap.WrapConfig

	if !panicwrap.Wrapped(&wrapConfig) {
		// Determine where logs should go in general (requested by the user)
		logWriter, err := logOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't setup log output: %s", err)
			return 1
		}
		if logWriter == nil {
			logWriter = ioutil.Discard
		}

		// We always send logs to a temporary file that we use in case
		// there is a panic. Otherwise, we delete it.
		logTempFile, err := ioutil.TempFile("", "terraform-log")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't setup logging tempfile: %s", err)
			return 1
		}
		defer os.Remove(logTempFile.Name())
		defer logTempFile.Close()

		// Tell the logger to log to this file
		os.Setenv(EnvLog, "")
		os.Setenv(EnvLogFile, "")

		// Setup the prefixed readers that send data properly to
		// stdout/stderr.
		doneCh := make(chan struct{})
		outR, outW := io.Pipe()
		go copyOutput(outR, doneCh)

		// Create the configuration for panicwrap and wrap our executable
		wrapConfig.Handler = panicHandler(logTempFile)
		wrapConfig.Writer = io.MultiWriter(logTempFile, logWriter)
		wrapConfig.Stdout = outW
		exitStatus, err := panicwrap.Wrap(&wrapConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Couldn't start Terraform: %s", err)
			return 1
		}

		// If >= 0, we're the parent, so just exit
		if exitStatus >= 0 {
			// Close the stdout writer so that our copy process can finish
			outW.Close()

			// Wait for the output copying to finish
			<-doneCh

			return exitStatus
		}

		// We're the child, so just close the tempfile we made in order to
		// save file handles since the tempfile is only used by the parent.
		logTempFile.Close()
	}

	// Call the real main
	return wrappedMain()
}

func wrappedMain() int {
	log.SetOutput(os.Stderr)

	// Load the configuration
	config := BuiltinConfig
	if err := config.Discover(); err != nil {
		fmt.Fprintf(os.Stderr, "Error discovering plugins: %s", err)
		return 1
	}

	// Make sure we clean up any managed plugins at the end of this
	defer plugin.CleanupClients()

	// Initialize the TFConfig settings for the commands...
	ContextOpts.Providers = config.ProviderFactories()
	ContextOpts.Provisioners = config.ProvisionerFactories()

	// Get the command line args. We shortcut "--version" and "-v" to
	// just show the version.
	args := os.Args[1:]
	for _, arg := range args {
		if arg == "-v" || arg == "-version" || arg == "--version" {
			newArgs := make([]string, len(args)+1)
			newArgs[0] = "version"
			copy(newArgs[1:], args)
			args = newArgs
			break
		}
	}

	cli := &cli.CLI{
		Args:       args,
		Commands:   Commands,
		HelpFunc:   cli.BasicHelpFunc("terraform"),
		HelpWriter: os.Stdout,
	}

	// Load the configuration file if we have one, that can be used to
	// define extra providers and provisioners.
	clicfgFile, err := cliConfigFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading CLI configuration: \n\n%s\n", err)
		return 1
	}

	if clicfgFile != "" {
		usrcfg, err := LoadConfig(clicfgFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading CLI configuration: \n\n%s\n", err)
			return 1
		}

		config = *config.Merge(usrcfg)
	}

	// Initialize the TFConfig settings for the commands...
	ContextOpts.Providers = config.ProviderFactories()
	ContextOpts.Provisioners = config.ProvisionerFactories()

	exitCode, err := cli.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing CLI: %s\n", err.Error())
		return 1
	}

	return exitCode
}

func cliConfigFile() (string, error) {
	mustExist := true
	configFilePath := os.Getenv("TERRAFORM_CONFIG")
	if configFilePath == "" {
		var err error
		configFilePath, err = configFile()
		mustExist = false

		if err != nil {
			log.Printf(
				"[ERROR] Error detecting default CLI config file path: %s",
				err)
		}
	}

	log.Printf("[DEBUG] Attempting to open CLI config file: %s", configFilePath)
	f, err := os.Open(configFilePath)
	if err == nil {
		f.Close()
		return configFilePath, nil
	}

	if mustExist || !os.IsNotExist(err) {
		return "", err
	}

	log.Println("[DEBUG] File doesn't exist, but doesn't need to. Ignoring.")
	return "", nil
}

// copyOutput uses output prefixes to determine whether data on stdout
// should go to stdout or stderr. This is due to panicwrap using stderr
// as the log and error channel.
func copyOutput(r io.Reader, doneCh chan<- struct{}) {
	defer close(doneCh)

	pr, err := prefixedio.NewReader(r)
	if err != nil {
		panic(err)
	}

	stderrR, err := pr.Prefix(ErrorPrefix)
	if err != nil {
		panic(err)
	}
	stdoutR, err := pr.Prefix(OutputPrefix)
	if err != nil {
		panic(err)
	}
	defaultR, err := pr.Prefix("")
	if err != nil {
		panic(err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		io.Copy(os.Stderr, stderrR)
	}()
	go func() {
		defer wg.Done()
		io.Copy(os.Stdout, stdoutR)
	}()
	go func() {
		defer wg.Done()
		io.Copy(os.Stdout, defaultR)
	}()

	wg.Wait()
}
