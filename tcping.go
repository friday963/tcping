package main

import (
	"bufio"
	"context"
	"flag"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/google/go-github/v45/github"
)

const (
	version    = "2.4.0"
	owner      = "pouriyajamshidi"
	repo       = "tcping"
	dnsTimeout = 2 * time.Second
)

// printer is a set of methods for printers to implement.
//
// Printers should NOT modify any existing data nor do any calculations.
// They should only perform visual operations on given data.
type printer interface {
	// printStart should print the first message, after the program starts.
	// This message is printed only once, at the very beginning.
	printStart(hostname string, port uint16)

	// printProbeSuccess should print a message after each successful probe.
	// hostname could be empty, meaning it's pinging an address.
	// streak is the number of successful consecutive probes.
	printProbeSuccess(hostname, ip string, port uint16, streak uint, rtt float32)

	// printProbeFail should print a message after each failed probe.
	// hostname could be empty, meaning it's pinging an address.
	// streak is the number of successful consecutive probes.
	printProbeFail(hostname, ip string, port uint16, streak uint)

	// printRetryingToResolve should print a message with the hostname
	// it is trying to resolve an ip for.
	//
	// This is only being printed when the -r flag is applied.
	printRetryingToResolve(hostname string)

	// printTotalDownTime should print a downtime duration.
	//
	// This is being called when host was unavailable for some time
	// but the latest probe was successful (became available).
	printTotalDownTime(downtime time.Duration)

	// printStatistics should print a message with
	// helpful statistics information.
	//
	// This is being called on exit and when user hits "Enter".
	printStatistics(s stats)

	// printVersion should print the current version.
	printVersion()

	// printInfo should a message, which is not directly related
	// to the pinging and serves as a helpful information.
	//
	// Example of such: new version with -u flag.
	printInfo(format string, args ...any)

	// printError should print an error message.
	// Printer should also apply \n to the given string, if needed.
	printError(format string, args ...any)
}

type stats struct {
	startTime                 time.Time
	endTime                   time.Time
	startOfUptime             time.Time
	startOfDowntime           time.Time
	lastSuccessfulProbe       time.Time
	lastUnsuccessfulProbe     time.Time
	printer                   printer      // printer holds the chosen printer implementation for outputting information and data.
	ticker                    *time.Ticker // ticker is used to handle time between probes.
	longestUptime             longestTime
	longestDowntime           longestTime
	rtt                       []float32
	hostnameChanges           []hostnameChange
	userInput                 userInput
	ongoingSuccessfulProbes   uint
	ongoingUnsuccessfulProbes uint
	totalDowntime             time.Duration
	totalUptime               time.Duration
	totalSuccessfulProbes     uint
	totalUnsuccessfulProbes   uint
	retriedHostnameLookups    uint
	rttResults                rttResult
	wasDown                   bool // wasDown is used to determine the duration of a downtime
	isIP                      bool // isIP suppresses printing the IP information twice when hostname is not provided
}

type userInput struct {
	ip                       netip.Addr
	hostname                 string
	networkInterface         networkInterface
	retryHostnameLookupAfter uint // Retry resolving target's hostname after a certain number of failed requests
	probesBeforeQuit         uint
	timeout                  time.Duration
	intervalBetweenProbes    time.Duration
	port                     uint16
	useIPv4                  bool
	useIPv6                  bool
	shouldRetryResolve       bool
}

type networkInterface struct {
	raddr  *net.TCPAddr
	dialer net.Dialer
	use    bool
}

type longestTime struct {
	start    time.Time
	end      time.Time
	duration time.Duration
}

type rttResult struct {
	min        float32
	max        float32
	average    float32
	hasResults bool
}

type hostnameChange struct {
	Addr netip.Addr `json:"addr,omitempty"`
	When time.Time  `json:"when,omitempty"`
}

// signalHandler catches SIGINT and SIGTERM then prints tcping stats
func signalHandler(tcpStats *stats) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		shutdown(tcpStats)
	}()
}

// monitorStdin checks stdin to see whether the 'Enter' key was pressed
func monitorStdin(stdinChan chan bool) {
	reader := bufio.NewReader(os.Stdin)
	for {
		input, _ := reader.ReadString('\n')

		if input == "\n" || input == "\r" || input == "\r\n" {
			stdinChan <- true
		}
	}
}

// printStats is a helper method for printStatistics
// for the current printer.
//
// This should be used instead, as it makes
// all the necessary calculations beforehand.
func (tcpStats *stats) printStats() {
	if tcpStats.wasDown {
		calcLongestDowntime(tcpStats, time.Since(tcpStats.startOfDowntime))
	} else {
		calcLongestUptime(tcpStats, time.Since(tcpStats.startOfUptime))
	}
	tcpStats.rttResults = calcMinAvgMaxRttTime(tcpStats.rtt)

	tcpStats.printer.printStatistics(*tcpStats)
}

// shutdown calculates endTime, prints statistics and calls os.Exit(0).
// This should be used as a main exit-point.
func shutdown(tcpStats *stats) {
	tcpStats.endTime = time.Now()
	tcpStats.printStats()

	// if the printer type is `database`, then close the db before
	// exiting to prevent any memory leaks
	if db, ok := tcpStats.printer.(database); ok {
		db.db.Close()
	}

	os.Exit(0)
}

// usage prints how tcping should be run
func usage() {
	executableName := os.Args[0]

	colorLightCyan("\nTCPING version %s\n\n", version)
	colorRed("Try running %s like:\n", executableName)
	colorRed("%s <hostname/ip> <port number>. For example:\n", executableName)
	colorRed("%s www.example.com 443\n", executableName)
	colorYellow("\n[optional flags]\n")

	flag.VisitAll(func(f *flag.Flag) {
		flagName := f.Name
		if len(f.Name) > 1 {
			flagName = "-" + flagName
		}

		colorYellow("  -%s : %s\n", flagName, f.Usage)
	})

	os.Exit(1)
}

// processUserInput gets and validate user input
func processUserInput(tcpStats *stats) {
	useIPv4 := flag.Bool("4", false, "only use IPv4.")
	useIPv6 := flag.Bool("6", false, "only use IPv6.")
	retryHostnameResolveAfter := flag.Uint("r", 0, "retry resolving target's hostname after <n> number of failed probes. e.g. -r 10 to retry after 10 failed probes.")
	probesBeforeQuit := flag.Uint("c", 0, "stop after <n> probes, regardless of the result. By default, no limit will be applied.")
	outputJSON := flag.Bool("j", false, "output in JSON format.")
	prettyJSON := flag.Bool("pretty", false, "use indentation when using json output format. No effect without the '-j' flag.")
	showVersion := flag.Bool("v", false, "show version.")
	shouldCheckUpdates := flag.Bool("u", false, "check for updates.")
	secondsBetweenProbes := flag.Float64("i", 1, "interval between sending probes. Real number allowed with dot as a decimal separator. The default is one second")
	timeout := flag.Float64("t", 1, "time to wait for a response, in seconds. Real number allowed. 0 means infinite timeout.")
	outputDb := flag.String("db", "", "path and file name to store tcping output to sqlite database.")
	interfaceName := flag.String("I", "", "interface name or address")

	flag.CommandLine.Usage = usage

	permuteArgs(os.Args[1:])
	flag.Parse()

	// validation for flag and args
	args := flag.Args()
	nFlag := flag.NFlag()

	// we need to set printers first, because they're used for
	// errors reporting and other output.
	if *outputJSON {
		tcpStats.printer = newJSONPrinter(*prettyJSON)
	} else if *outputDb != "" {
		tcpStats.printer = newDb(args, *outputDb)
	} else {
		tcpStats.printer = &planePrinter{}
	}

	// -u works on its own
	if *shouldCheckUpdates {
		if len(args) == 0 && nFlag == 1 {
			checkLatestVersion(tcpStats.printer)
		} else {
			usage()
		}
	}

	if *showVersion {
		tcpStats.printer.printVersion()
		os.Exit(0)
	}

	// host and port must be specified
	if len(args) != 2 {
		usage()
	}

	if *prettyJSON && !*outputJSON {
		tcpStats.printer.printError("--pretty has no effect without the -j flag.")
		usage()
	}

	if *useIPv4 && *useIPv6 {
		tcpStats.printer.printError("Only one IP version can be specified")
		usage()
	}

	if *retryHostnameResolveAfter > 0 {
		tcpStats.userInput.retryHostnameLookupAfter = *retryHostnameResolveAfter
	}

	if *useIPv4 {
		tcpStats.userInput.useIPv4 = true
	}

	if *useIPv6 {
		tcpStats.userInput.useIPv6 = true
	}

	// the non-flag command-line arguments
	port, err := strconv.ParseUint(args[1], 10, 16)
	if err != nil {
		tcpStats.printer.printError("Invalid port number: %s", args[1])
		os.Exit(1)
	}

	if port < 1 || port > 65535 {
		tcpStats.printer.printError("Port should be in 1..65535 range")
		os.Exit(1)
	}

	tcpStats.userInput.hostname = args[0]
	tcpStats.userInput.port = uint16(port)
	tcpStats.userInput.ip = resolveHostname(tcpStats)
	tcpStats.startTime = time.Now()
	tcpStats.userInput.probesBeforeQuit = *probesBeforeQuit
	tcpStats.userInput.timeout = secondsToDuration(*timeout)

	tcpStats.userInput.intervalBetweenProbes = secondsToDuration(*secondsBetweenProbes)
	if tcpStats.userInput.intervalBetweenProbes < 2*time.Millisecond {
		tcpStats.printer.printError("Wait interval should be more than 2 ms")
		os.Exit(1)
	}

	// this serves as a default starting value for tracking changes.
	tcpStats.hostnameChanges = []hostnameChange{
		{tcpStats.userInput.ip, time.Now()},
	}

	if tcpStats.userInput.hostname == tcpStats.userInput.ip.String() {
		tcpStats.isIP = true
	}

	if tcpStats.userInput.retryHostnameLookupAfter > 0 && !tcpStats.isIP {
		tcpStats.userInput.shouldRetryResolve = true
	}

	if *interfaceName != "" {
		tcpStats.userInput.networkInterface = newNetworkInterface(tcpStats, *interfaceName)
	}
}

/*
permuteArgs permute args for flag parsing stops just before the first non-flag argument.

see: https://pkg.go.dev/flag
*/
func permuteArgs(args []string) {
	var flagArgs []string
	var nonFlagArgs []string

	for i := 0; i < len(args); i++ {
		v := args[i]
		if v[0] == '-' {
			var optionName string
			if v[1] == '-' {
				optionName = v[2:]
			} else {
				optionName = v[1:]
			}
			switch optionName {
			case "c":
				fallthrough
			case "t":
				fallthrough
			case "db":
				fallthrough
			case "I":
				fallthrough
			case "i":
				fallthrough
			case "r":
				/* out of index */
				if len(args) <= i+1 {
					usage()
				}
				/* the next flag has come */
				optionVal := args[i+1]
				if optionVal[0] == '-' {
					usage()
				}
				flagArgs = append(flagArgs, args[i:i+2]...)
				i++
			default:
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			nonFlagArgs = append(nonFlagArgs, args[i])
		}
	}
	permutedArgs := append(flagArgs, nonFlagArgs...)

	/* replace args */
	for i := 0; i < len(args); i++ {
		args[i] = permutedArgs[i]
	}
}

// newNetworkInterface uses the 1st ip address of the interface
// if any err occurs it calls `tcpStats.printer.printError` and exits with statuscode 1.
// or return `networkInterface`
func newNetworkInterface(tcpStats *stats, netInterface string) networkInterface {
	var interfaceAddress net.IP

	// if netinterface is the addres `interfaceAddress` var will not be `nil`
	interfaceAddress = net.ParseIP(netInterface)

	if interfaceAddress == nil {
		ief, err := net.InterfaceByName(netInterface)
		if err != nil {
			tcpStats.printer.printError("Interface %s not found", netInterface)
			os.Exit(1)
		}

		addrs, err := ief.Addrs()
		if err != nil {
			tcpStats.printer.printError("Unable to get Interface addresses")
			os.Exit(1)
		}

		// Iterating through the available addresses to identify valid IP configurations
		for _, addr := range addrs {
			if ip := addr.(*net.IPNet).IP; ip != nil {
				// netip.Addr
				nipAddr, err := netip.ParseAddr(ip.String())
				if err != nil {
					continue
				}

				if nipAddr.Is4() && !tcpStats.userInput.useIPv6 {
					interfaceAddress = ip
					break
				} else if nipAddr.Is6() && !tcpStats.userInput.useIPv4 {
					if nipAddr.IsLinkLocalUnicast() {
						continue
					}
					interfaceAddress = ip
					break
				}
			}
		}

		if interfaceAddress == nil {
			tcpStats.printer.printError("Unable to get Interface's IP Address")
			os.Exit(1)
		}
	}

	// Initializing a networkInterface struct and setting the 'use' field to true
	ni := networkInterface{
		use: true,
	}

	// remote address
	ni.raddr = &net.TCPAddr{
		IP:   net.ParseIP(tcpStats.userInput.ip.String()),
		Port: int(tcpStats.userInput.port),
	}

	// local address
	laddr := &net.TCPAddr{
		IP: interfaceAddress,
	}

	ni.dialer = net.Dialer{
		LocalAddr: laddr,
		Timeout:   tcpStats.userInput.timeout, // Set the timeout duration
	}

	return ni
}

// checkLatestVersion checks for updates and print a message
func checkLatestVersion(p printer) {
	c := github.NewClient(nil)

	/* unauthenticated requests from the same IP are limited to 60 per hour. */
	latestRelease, _, err := c.Repositories.GetLatestRelease(context.Background(), owner, repo)
	if err != nil {
		p.printError("Failed to check for updates %s", err.Error())
		os.Exit(1)
	}

	reg := `^v?(\d+\.\d+\.\d+)$`
	latestTagName := latestRelease.GetTagName()
	latestVersion := regexp.MustCompile(reg).FindStringSubmatch(latestTagName)

	if len(latestVersion) == 0 {
		p.printError("Failed to check for updates. The version name does not match the rule: %s", latestTagName)
		os.Exit(1)
	}

	if latestVersion[1] != version {
		p.printInfo("Found newer version %s", latestVersion[1])
		p.printInfo("Please update TCPING from the URL below:")
		p.printInfo("https://github.com/%s/%s/releases/tag/%s",
			owner, repo, latestTagName)
	} else {
		p.printInfo("Newer version not found. %s is the latest version.",
			version)
	}
	os.Exit(0)
}

// selectResolvedIP returns a single IPv4 or IPv6 address from the net.IP slice of resolved addresses
func selectResolvedIP(tcpStats *stats, ipAddrs []netip.Addr) netip.Addr {
	var index int
	var ipList []netip.Addr
	var ip netip.Addr

	switch {
	case tcpStats.userInput.useIPv4:
		for _, ip := range ipAddrs {
			if ip.Is4() {
				ipList = append(ipList, ip)
			}
		}

		if len(ipList) == 0 {
			tcpStats.printer.printError("Failed to find IPv4 address for %s", tcpStats.userInput.hostname)
			os.Exit(1)
		}

		if len(ipList) > 1 {
			index = rand.Intn(len(ipList))
		} else {
			index = 0
		}

		ip, _ = netip.ParseAddr(ipList[index].Unmap().String())

	case tcpStats.userInput.useIPv6:
		for _, ip := range ipAddrs {
			if ip.Is6() {
				ipList = append(ipList, ip)
			}
		}

		if len(ipList) == 0 {
			tcpStats.printer.printError("Failed to find IPv6 address for %s", tcpStats.userInput.hostname)
			os.Exit(1)
		}

		if len(ipList) > 1 {
			index = rand.Intn(len(ipList))
		} else {
			index = 0
		}

		ip, _ = netip.ParseAddr(ipList[index].Unmap().String())

	default:
		if len(ipAddrs) > 1 {
			index = rand.Intn(len(ipAddrs))
		} else {
			index = 0
		}

		ip, _ = netip.ParseAddr(ipAddrs[index].Unmap().String())
	}

	return ip
}

// resolveHostname handles hostname resolution with a timeout value of a second
func resolveHostname(tcpStats *stats) netip.Addr {
	ip, err := netip.ParseAddr(tcpStats.userInput.hostname)
	if err == nil {
		return ip
	}

	ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
	defer cancel()

	ipAddrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", tcpStats.userInput.hostname)

	// Prevent tcping to exit if it has been running for a while
	if err != nil && (tcpStats.totalSuccessfulProbes != 0 || tcpStats.totalUnsuccessfulProbes != 0) {
		return tcpStats.userInput.ip
	} else if err != nil {
		tcpStats.printer.printError("Failed to resolve %s: %s", tcpStats.userInput.hostname, err)
		os.Exit(1)
	}

	return selectResolvedIP(tcpStats, ipAddrs)
}

// retryResolveHostname retries resolving a hostname after certain number of failures
func retryResolveHostname(tcpStats *stats) {
	if tcpStats.ongoingUnsuccessfulProbes >= tcpStats.userInput.retryHostnameLookupAfter {
		tcpStats.printer.printRetryingToResolve(tcpStats.userInput.hostname)
		tcpStats.userInput.ip = resolveHostname(tcpStats)
		tcpStats.ongoingUnsuccessfulProbes = 0
		tcpStats.retriedHostnameLookups += 1

		// At this point hostnameChanges should have len > 0, but just in case
		if len(tcpStats.hostnameChanges) == 0 {
			return
		}

		lastAddr := tcpStats.hostnameChanges[len(tcpStats.hostnameChanges)-1].Addr
		if lastAddr != tcpStats.userInput.ip {
			tcpStats.hostnameChanges = append(tcpStats.hostnameChanges, hostnameChange{
				Addr: tcpStats.userInput.ip,
				When: time.Now(),
			})
		}
	}
}

// newLongestTime creates LongestTime structure
func newLongestTime(startTime time.Time, duration time.Duration) longestTime {
	return longestTime{
		start:    startTime,
		end:      startTime.Add(duration),
		duration: duration,
	}
}

// calcMinAvgMaxRttTime calculates min, avg and max RTT values
func calcMinAvgMaxRttTime(timeArr []float32) rttResult {
	var sum float32
	var result rttResult

	arrLen := len(timeArr)
	// rttResults.min = ^uint(0.0)
	if arrLen > 0 {
		result.min = timeArr[0]
	}

	for i := 0; i < arrLen; i++ {
		sum += timeArr[i]

		if timeArr[i] > result.max {
			result.max = timeArr[i]
		}

		if timeArr[i] < result.min {
			result.min = timeArr[i]
		}
	}

	if arrLen > 0 {
		result.hasResults = true
		result.average = sum / float32(arrLen)
	}

	return result
}

// calcLongestUptime calculates the longest uptime and sets it to tcpStats.
func calcLongestUptime(tcpStats *stats, duration time.Duration) {
	if tcpStats.startOfUptime.IsZero() || duration == 0 {
		return
	}

	longestUptime := newLongestTime(tcpStats.startOfUptime, duration)

	// It means it is the first time we're calling this function
	if tcpStats.longestUptime.end.IsZero() {
		tcpStats.longestUptime = longestUptime
		return
	}

	if longestUptime.duration >= tcpStats.longestUptime.duration {
		tcpStats.longestUptime = longestUptime
	}
}

// calcLongestDowntime calculates the longest downtime and sets it to tcpStats.
func calcLongestDowntime(tcpStats *stats, duration time.Duration) {
	if tcpStats.startOfDowntime.IsZero() || duration == 0 {
		return
	}

	longestDowntime := newLongestTime(tcpStats.startOfDowntime, duration)

	// It means it is the first time we're calling this function
	if tcpStats.longestDowntime.end.IsZero() {
		tcpStats.longestDowntime = longestDowntime
		return
	}

	if longestDowntime.duration >= tcpStats.longestDowntime.duration {
		tcpStats.longestDowntime = longestDowntime
	}
}

// nanoToMillisecond returns an amount of milliseconds from nanoseconds.
// Using duration.Milliseconds() is not an option, because it drops
// decimal points, returning an int.
func nanoToMillisecond(nano int64) float32 {
	return float32(nano) / float32(time.Millisecond)
}

// secondsToDuration returns the corresonding duration from seconds expressed with a float.
func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(1000*seconds) * time.Millisecond
}

// maxDuration is the implementation of the math.Max function for time.Duration types.
// returns the longest duration of x or y.
func maxDuration(x, y time.Duration) time.Duration {
	if x > y {
		return x
	}
	return y
}

// handleConnError processes failed probes
func (tcpStats *stats) handleConnError(connTime time.Time, elapsed time.Duration) {
	if !tcpStats.wasDown {
		tcpStats.startOfDowntime = connTime
		uptime := tcpStats.startOfDowntime.Sub(tcpStats.startOfUptime)
		calcLongestUptime(tcpStats, uptime)
		tcpStats.startOfUptime = time.Time{}
		tcpStats.wasDown = true
	}

	tcpStats.totalDowntime += elapsed
	tcpStats.lastUnsuccessfulProbe = connTime
	tcpStats.totalUnsuccessfulProbes += 1
	tcpStats.ongoingUnsuccessfulProbes += 1

	tcpStats.printer.printProbeFail(
		tcpStats.userInput.hostname,
		tcpStats.userInput.ip.String(),
		tcpStats.userInput.port,
		tcpStats.ongoingUnsuccessfulProbes,
	)
}

// handleConnSuccess processes successful probes
func (tcpStats *stats) handleConnSuccess(rtt float32, connTime time.Time, elapsed time.Duration) {
	if tcpStats.wasDown {
		tcpStats.startOfUptime = connTime
		downtime := tcpStats.startOfUptime.Sub(tcpStats.startOfDowntime)
		calcLongestDowntime(tcpStats, downtime)
		tcpStats.printer.printTotalDownTime(downtime)
		tcpStats.startOfDowntime = time.Time{}
		tcpStats.wasDown = false
		tcpStats.ongoingUnsuccessfulProbes = 0
		tcpStats.ongoingSuccessfulProbes = 0
	}

	if tcpStats.startOfUptime.IsZero() {
		tcpStats.startOfUptime = connTime
	}

	tcpStats.totalUptime += elapsed
	tcpStats.lastSuccessfulProbe = connTime
	tcpStats.totalSuccessfulProbes += 1
	tcpStats.ongoingSuccessfulProbes += 1
	tcpStats.rtt = append(tcpStats.rtt, rtt)

	tcpStats.printer.printProbeSuccess(
		tcpStats.userInput.hostname,
		tcpStats.userInput.ip.String(),
		tcpStats.userInput.port,
		tcpStats.ongoingSuccessfulProbes,
		rtt,
	)
}

// tcping pings a host, TCP style
func tcping(tcpStats *stats) {
	var err error
	var conn net.Conn
	connStart := time.Now()

	if tcpStats.userInput.networkInterface.use {
		// dialer already contains the timeout value
		conn, err = tcpStats.userInput.networkInterface.dialer.Dial("tcp", tcpStats.userInput.networkInterface.raddr.String())
	} else {
		IPAndPort := netip.AddrPortFrom(tcpStats.userInput.ip, tcpStats.userInput.port)
		conn, err = net.DialTimeout("tcp", IPAndPort.String(), tcpStats.userInput.timeout)
	}

	connDuration := time.Since(connStart)
	rtt := nanoToMillisecond(connDuration.Nanoseconds())

	elapsed := maxDuration(connDuration, tcpStats.userInput.intervalBetweenProbes)

	if err != nil {
		tcpStats.handleConnError(connStart, elapsed)
	} else {
		tcpStats.handleConnSuccess(rtt, connStart, elapsed)
		conn.Close()
	}
	<-tcpStats.ticker.C

}

func main() {
	tcpStats := &stats{}
	processUserInput(tcpStats)
	tcpStats.ticker = time.NewTicker(tcpStats.userInput.intervalBetweenProbes)
	defer tcpStats.ticker.Stop()

	signalHandler(tcpStats)

	tcpStats.printer.printStart(tcpStats.userInput.hostname, tcpStats.userInput.port)

	stdinChan := make(chan bool)
	go monitorStdin(stdinChan)

	var probeCount uint = 0
	for {
		if tcpStats.userInput.shouldRetryResolve {
			retryResolveHostname(tcpStats)
		}

		tcping(tcpStats)

		select {
		case pressedEnter := <-stdinChan:
			if pressedEnter {
				tcpStats.printStats()
			}
		default:
		}

		if tcpStats.userInput.probesBeforeQuit != 0 {
			probeCount++
			if probeCount == tcpStats.userInput.probesBeforeQuit {
				shutdown(tcpStats)
			}
		}
	}
}
