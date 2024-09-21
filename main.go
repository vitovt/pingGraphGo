package main

import (
    "flag"
    "fmt"
    "math"
    "net"
    "os"
    "os/signal"
    "runtime"
    "sync"
    "syscall"
    "time"

    termui "github.com/gizak/termui/v3"
    "github.com/gizak/termui/v3/widgets"
    "golang.org/x/net/icmp"
    "golang.org/x/net/ipv4"
    "golang.org/x/net/ipv6"
)

func main() {
    // Parse command-line arguments
    var (
        timeout     = flag.Int("W", 150, "Timeout in milliseconds for each ping request")
        interval    = flag.Float64("i", 0.1, "Interval between pings in seconds")
        deadTimeout = flag.Float64("D", 500, "Execution timeout in milliseconds for each ping command (max 10000 ms)")
        useIPv6     = flag.Bool("6", false, "Use IPv6 for the ping")
    )
    flag.Parse()

    if len(flag.Args()) < 1 {
        fmt.Println("Usage: go run main.go [options] host")
        flag.PrintDefaults()
        os.Exit(1)
    }
    host := flag.Args()[0]

    if *deadTimeout > 10000 || *deadTimeout < float64(*timeout) {
        fmt.Printf("Dead timeout (-D) value %v out of range. Exiting.\n", *deadTimeout)
        os.Exit(1)
    }

    resolvedHost, err := resolveHostname(host, *useIPv6)
    if err != nil {
        fmt.Printf("Could not resolve host %s. Exiting.\n", host)
        os.Exit(1)
    }

    // Initialize variables
    var times []float64
    var pings []int
    var mutex sync.Mutex
    running := true
    currentScale := "linear"
    pingCount := 0

    startTime := time.Now()

    // Start the ping goroutine
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        defer wg.Done()
        ping(resolvedHost, &times, &pings, &mutex, *timeout, *deadTimeout, *interval, &running, &pingCount, *useIPv6)
    }()

    // Initialize termui
    if err := termui.Init(); err != nil {
        fmt.Printf("Failed to initialize termui: %v\n", err)
        os.Exit(1)
    }
    defer termui.Close()

    // Create UI elements
    plot := widgets.NewPlot()
    plot.Title = fmt.Sprintf("Ping response times to %s%s", func() string {
        if *useIPv6 {
            return "IPv6 "
        }
        return "IPv4 "
    }(), host)
    plot.Data = make([][]float64, 1)
    plot.Marker = widgets.MarkerBraille
    plot.LineColors[0] = termui.ColorGreen

    // Create stats paragraph
    statsParagraph := widgets.NewParagraph()
    statsParagraph.Title = "Statistics"
    statsParagraph.Text = "Calculating..."

    // Set up grid layout
    grid := termui.NewGrid()
    termWidth, termHeight := termui.TerminalDimensions()
    grid.SetRect(0, 0, termWidth, termHeight)

    grid.Set(
        termui.NewRow(0.7, plot),
        termui.NewRow(0.3, statsParagraph),
    )

    // Handle events
    uiEvents := termui.PollEvents()
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()

    // Handle Ctrl+C and 'q' to quit
    sigs := make(chan os.Signal, 1)
    signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
    go func() {
        <-sigs
        running = false
        termui.Close()
        fmt.Println("Exiting...")
        os.Exit(0)
    }()

    for running {
        select {
        case e := <-uiEvents:
            switch e.Type {
            case termui.KeyboardEvent:
                switch e.ID {
                case "q", "<C-c>":
                    running = false
                    termui.Close()
                    fmt.Println("Exiting...")
                    os.Exit(0)
                case "l":
                    if currentScale == "linear" {
                        currentScale = "log"
                    } else {
                        currentScale = "linear"
                    }
                }
            case termui.ResizeEvent:
                payload := e.Payload.(termui.Resize)
                grid.SetRect(0, 0, payload.Width, payload.Height)
                termui.Clear()
            }
        case <-ticker.C:
            // Update plot and stats
            mutex.Lock()
            plotData := make([]float64, len(times))
            copy(plotData, times)
            mutex.Unlock()

            if len(plotData) > 0 {
                if currentScale == "log" {
                    transformedData := make([]float64, len(plotData))
                    for i, v := range plotData {
                        if v > 0 {
                            transformedData[i] = math.Log10(v)
                        } else {
                            transformedData[i] = 0
                        }
                    }
                    plot.Data[0] = transformedData
                    plot.MaxVal = maxFloat64(transformedData)
                    // plot.MinVal is not available; termui handles MinVal internally
                } else {
                    plot.Data[0] = plotData
                    plot.MaxVal = maxFloat64(plotData)
                    // plot.MinVal is not available; termui handles MinVal internally
                }
            }

            // Update stats
            statsText := updateStats(&times, *timeout, *deadTimeout, startTime, *interval)
            statsParagraph.Text = statsText

            if len(plotData) >= 2 {
                // [update plot data and render]
                // Render UI
                termui.Render(grid)
            } else {
                // Only update stats
                statsText := updateStats(&times, *timeout, *deadTimeout, startTime, *interval)
                statsParagraph.Text = statsText
            }
          }
    }
    wg.Wait()
}

func resolveHostname(host string, useIPv6 bool) (string, error) {
    var ipAddr string
    ips, err := net.LookupIP(host)
    if err != nil {
        return "", fmt.Errorf("Failed to resolve hostname %s with error: %v", host, err)
    }
    for _, ip := range ips {
        if useIPv6 && ip.To16() != nil && ip.To4() == nil {
            ipAddr = ip.String()
            break
        } else if !useIPv6 && ip.To4() != nil {
            ipAddr = ip.String()
            break
        }
    }
    if ipAddr == "" {
        return "", fmt.Errorf("No %s address found for host %s", func() string {
            if useIPv6 {
                return "IPv6"
            }
            return "IPv4"
        }(), host)
    }
    return ipAddr, nil
}

func ping(host string, times *[]float64, pings *[]int, mutex *sync.Mutex, timeout int, deadTimeout float64, interval float64, running *bool, pingCount *int, useIPv6 bool) {
    var network string
    if runtime.GOOS == "windows" {
        if useIPv6 {
            network = "ip6:ipv6-icmp"
        } else {
            network = "ip4:icmp"
        }
    } else {
        if useIPv6 {
            network = "ip6:ipv6-icmp"
        } else {
            network = "ip4:icmp"
        }
    }

    conn, err := icmp.ListenPacket(network, "")
    if err != nil {
        fmt.Printf("Error listening to ICMP: %v\n", err)
        *running = false
        return
    }
    defer conn.Close()

    id := os.Getpid() & 0xffff

    for *running {
        *pingCount++
        var msg *icmp.Message
        if useIPv6 {
            msg = &icmp.Message{
                Type: ipv6.ICMPTypeEchoRequest,
                Code: 0,
                Body: &icmp.Echo{
                    ID:   id,
                    Seq:  *pingCount,
                    Data: []byte("HELLO-PING"),
                },
            }
        } else {
            msg = &icmp.Message{
                Type: ipv4.ICMPTypeEcho,
                Code: 0,
                Body: &icmp.Echo{
                    ID:   id,
                    Seq:  *pingCount,
                    Data: []byte("HELLO-PING"),
                },
            }
        }

        msgBytes, err := msg.Marshal(nil)
        if err != nil {
            fmt.Printf("Error marshalling ICMP message: %v\n", err)
            *running = false
            return
        }

        destAddr := &net.IPAddr{IP: net.ParseIP(host)}

        start := time.Now()
        n, err := conn.WriteTo(msgBytes, destAddr)
        if err != nil {
            fmt.Printf("Error sending ICMP request: %v\n", err)
            mutex.Lock()
            *times = append(*times, deadTimeout)
            *pings = append(*pings, *pingCount)
            mutex.Unlock()
            time.Sleep(time.Duration(interval * float64(time.Second)))
            continue
        }

        if n != len(msgBytes) {
            fmt.Printf("Sent %d bytes, expected to send %d bytes\n", n, len(msgBytes))
        }

        conn.SetReadDeadline(time.Now().Add(time.Duration(timeout) * time.Millisecond))
        reply := make([]byte, 1500)
        n, peer, err := conn.ReadFrom(reply)
        duration := time.Since(start)

        if err != nil {
            if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
                fmt.Printf("Ping to %s timed out\n", host)
                mutex.Lock()
                *times = append(*times, deadTimeout)
                *pings = append(*pings, *pingCount)
                mutex.Unlock()
            } else {
                fmt.Printf("Error receiving ICMP reply: %v\n", err)
                mutex.Lock()
                *times = append(*times, deadTimeout)
                *pings = append(*pings, *pingCount)
                mutex.Unlock()
            }
        } else {
            // Parse reply
            var protocol int
            if useIPv6 {
                protocol = ipv6.ICMPTypeEchoReply.Protocol()
            } else {
                protocol = ipv4.ICMPTypeEchoReply.Protocol()
            }
            receivedMsg, err := icmp.ParseMessage(protocol, reply[:n])
            if err != nil {
                fmt.Printf("Error parsing ICMP reply: %v\n", err)
                mutex.Lock()
                *times = append(*times, deadTimeout)
                *pings = append(*pings, *pingCount)
                mutex.Unlock()
            } else {
                switch receivedMsg.Type {
                case ipv4.ICMPTypeEchoReply, ipv6.ICMPTypeEchoReply:
                    delay := float64(duration.Milliseconds())
                    mutex.Lock()
                    *times = append(*times, delay)
                    *pings = append(*pings, *pingCount)
                    mutex.Unlock()
                    if delay > float64(timeout) {
                        fmt.Printf("Ping response time %.2f ms exceeded timeout of %d ms\n", delay, timeout)
                    }
                default:
                    fmt.Printf("Received non-echo reply from %v: %+v\n", peer, receivedMsg)
                    mutex.Lock()
                    *times = append(*times, deadTimeout)
                    *pings = append(*pings, *pingCount)
                    mutex.Unlock()
                }
            }
        }

        time.Sleep(time.Duration(interval * float64(time.Second)))
    }
}

func updateStats(times *[]float64, timeout int, deadTimeout float64, startTime time.Time, interval float64) string {
    totalRunningTime := time.Since(startTime).Seconds()
    validTimes := []float64{}
    for _, t := range *times {
        if t != deadTimeout {
            validTimes = append(validTimes, t)
        }
    }

    var avgTime, minTime, maxTime, stdDev, jitter float64
    if len(validTimes) > 0 {
        sum := 0.0
        for _, t := range validTimes {
            sum += t
        }
        avgTime = sum / float64(len(validTimes))

        minTime = validTimes[0]
        maxTime = validTimes[0]
        for _, t := range validTimes {
            if t < minTime {
                minTime = t
            }
            if t > maxTime {
                maxTime = t
            }
        }

        // Calculate standard deviation
        sumSquares := 0.0
        for _, t := range validTimes {
            sumSquares += (t - avgTime) * (t - avgTime)
        }
        stdDev = math.Sqrt(sumSquares / float64(len(validTimes)))

        // Calculate jitter
        if len(validTimes) > 1 {
            sumDiffs := 0.0
            for i := 1; i < len(validTimes); i++ {
                sumDiffs += math.Abs(validTimes[i] - validTimes[i-1])
            }
            jitter = sumDiffs / float64(len(validTimes)-1)
        }
    }

    // Calculate percentage greater than timeout
    timesGreaterThanTimeout := 0
    timesLost := 0
    for _, t := range *times {
        if t > float64(timeout) && t != deadTimeout {
            timesGreaterThanTimeout++
        }
        if t == deadTimeout {
            timesLost++
        }
    }
    percentageGreaterThanTimeout := 0.0
    percentageLost := 0.0
    if len(*times) > 0 {
        percentageGreaterThanTimeout = float64(timesGreaterThanTimeout) / float64(len(*times)) * 100
        percentageLost = float64(timesLost) / float64(len(*times)) * 100
    }

    // Calculate maximum sequential number of times >= timeout
    maxSequentialTimeout := 0
    currentSequenceTimeout := 0
    totalTimeout := 0
    for _, t := range *times {
        if t >= float64(timeout) && t != deadTimeout {
            totalTimeout++
            currentSequenceTimeout++
        } else if t == deadTimeout {
            totalTimeout++
            currentSequenceTimeout++
        } else {
            if currentSequenceTimeout > maxSequentialTimeout {
                maxSequentialTimeout = currentSequenceTimeout
            }
            currentSequenceTimeout = 0
        }
    }
    if currentSequenceTimeout > maxSequentialTimeout {
        maxSequentialTimeout = currentSequenceTimeout
    }

    statsText := fmt.Sprintf(
        "Average: %.2f ms\nMax: %.2f ms\nMin: %.2f ms\nStd Dev: %.2f ms\nJitter: %.2f ms\n%% Timeout(>): %.2f%%\n%% Lost(=): %.2f%%\nTotal N: %d\nN timeout: %d\nMax N SEQ tim.: %d\nN lost: %d\n---settings---\n-W timeout: %d ms\n-D: %.0f ms\n-i interval: %.2f s\n\nRunTime: %.2f s\n\nPress 'q' to quit\nPress 'l' to toggle scale",
        avgTime, maxTime, minTime, stdDev, jitter, percentageGreaterThanTimeout, percentageLost, len(*times), totalTimeout, maxSequentialTimeout, timesLost, timeout, deadTimeout, interval, totalRunningTime)
    return statsText
}

func maxFloat64(slice []float64) float64 {
    max := slice[0]
    for _, v := range slice {
        if v > max {
            max = v
        }
    }
    return max
}

func minFloat64(slice []float64) float64 {
    min := slice[0]
    for _, v := range slice {
        if v < min {
            min = v
        }
    }
    return min
}

