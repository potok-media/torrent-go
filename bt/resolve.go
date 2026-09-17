package bt

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

var defaultTrackers = [][]string{
	{"http://bt.t-ru.org/ann?magnet"},
	{"http://bt2.t-ru.org/ann?magnet"},
	{"http://bt3.t-ru.org/ann?magnet"},
	{"http://bt4.t-ru.org/ann?magnet"},
	{"http://nyaa.tracker.wf:7777/announce"},
	{"udp://tracker.opentrackr.org:1337/announce"},
	{"udp://open.stealth.si:80/announce"},
	{"udp://tracker.torrent.eu.org:451/announce"},
	{"udp://tracker.dler.com:6969/announce"},
	{"udp://opentor.org:2710/announce"},
	{"udp://open.demonii.com:1337/announce"},
	{"udp://p4p.arenabg.com:1337/announce"},
	{"udp://tracker.coppersurfer.tk:6969/announce"},
	{"udp://exodus.desync.com:6969/announce"},
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	
	if ip4 := ip.To4(); ip4 != nil {
		return ip4[0] == 10 ||
			(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
			(ip4[0] == 192 && ip4[1] == 168)
	}
	
	return len(ip) == 16 && (ip[0]&0xfe) == 0xfc
}

func secureResolveHost(urlStr string) error {
	u, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return errors.New("SSRF protection: private IP addresses are not allowed")
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("failed to resolve host %s: %w", host, err)
	}

	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("SSRF protection: host %s resolves to private IP %s", host, ip.String())
		}
	}

	return nil
}

func ResolveTorrent(ctx context.Context, client *torrent.Client, link string) (*torrent.Torrent, error) {
	var t *torrent.Torrent
	var err error

	linkLower := strings.ToLower(link)

	if strings.HasPrefix(linkLower, "magnet:") {
		t, err = client.AddMagnet(link)
		if err != nil {
			return nil, fmt.Errorf("failed to add magnet: %w", err)
		}
	} else if strings.HasPrefix(linkLower, "http://") || strings.HasPrefix(linkLower, "https://") {
		if err := secureResolveHost(link); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
		if err != nil {
			return nil, err
		}

		httpClient := &http.Client{
			Timeout: 10 * time.Second,
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch torrent file: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		}

		limitReader := io.LimitReader(resp.Body, 5*1024*1024)
		torrentBytes, err := io.ReadAll(limitReader)
		if err != nil {
			return nil, fmt.Errorf("failed to read torrent body: %w", err)
		}

		mi, err := metainfo.Load(bytes.NewReader(torrentBytes))
		if err != nil {
			return nil, fmt.Errorf("failed to parse torrent metainfo: %w", err)
		}

		t, err = client.AddTorrent(mi)
		if err != nil {
			return nil, fmt.Errorf("failed to add torrent: %w", err)
		}
	} else if len(link) == 40 || len(link) == 64 {
		var h metainfo.Hash
		hexBytes, err := hex.DecodeString(link[:40])
		if err != nil {
			return nil, fmt.Errorf("failed to parse raw infohash: %w", err)
		}
		copy(h[:], hexBytes)

		var ok bool
		t, ok = client.Torrent(h)
		if !ok {
			magnetUri := "magnet:?xt=urn:btih:" + link[:40]
			t, err = client.AddMagnet(magnetUri)
			if err != nil {
				return nil, fmt.Errorf("failed to add magnet from infohash: %w", err)
			}
		}
	} else if _, err := os.Stat(link); err == nil {
		mi, err := metainfo.LoadFromFile(link)
		if err != nil {
			return nil, fmt.Errorf("failed to load local torrent file: %w", err)
		}
		t, err = client.AddTorrent(mi)
		if err != nil {
			return nil, fmt.Errorf("failed to add local torrent: %w", err)
		}
	} else {
		return nil, errors.New("unsupported torrent link format or file not found")
	}

	t.AddTrackers(defaultTrackers)

	return t, nil
}
