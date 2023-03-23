package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/grafov/m3u8"
	flag "github.com/spf13/pflag"
)

// Variables used to store the sent command-line flags.
var (
	saveSegments bool
	manifestURI  string
	manifestType string
)

func init() {
	flag.BoolVarP(
		&saveSegments,
		"save",
		"s",
		false,
		"when present, all segments will be saved, and not only error segments",
	)
	flag.StringVarP(
		&manifestURI,
		"manifest",
		"m",
		"",
		"master manifest uri to be called. If uri isn't signed, a manifest token will be required",
	)
	flag.StringVarP(
		&manifestType,
		"type",
		"y",
		"master",
		"OPTIONAL, can be \"master\" or \"media\" types",
	)
}

func main() {
	flag.Parse()

	if manifestURI == "" {
		log.Fatal(newError("no manifest uri provided").Error())
	}

	if strings.Contains(manifestURI, "deploys.brightcove.com") && manifestTKN == "" {
		log.Fatal(newError("no token provided on gantry request").Error())
	}

	pc := PlaylistClient{
		client: &http.Client{},
	}

	if err := pc.Start(); err != nil {
		log.Fatal(err.Error())
	}
	fmt.Println("\nDone!")
}

type PlaylistClient struct {
	client *http.Client
}

func (pc *PlaylistClient) Start() error {
	var err error
	switch manifestType {
	case "master":
		err = pc.GetMaster(manifestURI)
	case "media":
		err = pc.GetMedia(manifestURI, "media")
	default:
		return newError("type \"" + manifestType + "\" isn't supported")
	}
	return err
}

func (pc *PlaylistClient) GetMaster(uri string) error {
	p, pType, err := pc.GetPlaylist(uri)
	if err != nil {
		return err
	}

	if pType != m3u8.MASTER {
		return newError("manifest must be of master type")
	}

	mp, ok := p.(*m3u8.MasterPlaylist)
	if !ok {
		return newError("unable to parse master manifest")
	}

	var wg sync.WaitGroup
	for i, variant := range mp.Variants {
		if variant.Iframe {
			continue
		}

		wg.Add(1)
		go func(i int, variant *m3u8.Variant) {
			defer wg.Done()
			if err = pc.GetMedia(variant.URI, fmt.Sprintf("video_%d", i)); err != nil {
				log.Fatal(err.Error())
			}
		}(i, variant)

		if variant.Alternatives == nil {
			continue
		}

		for j, alt := range variant.Alternatives {
			wg.Add(1)
			go func(i, j int, alt *m3u8.Alternative) {
				defer wg.Done()
				if err = pc.GetMedia(alt.URI, fmt.Sprintf("audio_%d_%d", i, j)); err != nil {
					log.Fatal(err.Error())
				}
			}(i, j, alt)
		}

	}
	wg.Wait()
	return nil
}

func (pc *PlaylistClient) GetMedia(uri string, folder string) error {
	p, pType, err := pc.GetPlaylist(uri)
	if err != nil {
		return err
	}

	if pType != m3u8.MEDIA {
		return newError("manifest must be of media type")
	}

	mp, ok := p.(*m3u8.MediaPlaylist)
	if !ok {
		return newError("unable to parse media manifest")
	}

	mode, err := pc.GetCBCDecrypter(mp.Key.URI, mp.Key.IV)
	if err != nil {
		return err
	}

	// Clear dir and create again
	if err = os.RemoveAll(folder); err != nil {
		return err
	}

	fmt.Printf("Starting decryption for: %s\n", uri)

	var wg sync.WaitGroup
	for i := 0; i < int(mp.Count()); i++ {
		if mp.Segments[i] == nil {
			continue
		}
		wg.Add(1)
		go func(iter int) {
			defer wg.Done()
			if err = pc.DecodeSegment(mp.Segments[iter].URI, mode, folder, iter); err != nil {
				log.Fatal(err.Error())
			}
		}(i)
	}
	wg.Wait()
	return nil
}

func (pc *PlaylistClient) GetPlaylist(uri string) (m3u8.Playlist, m3u8.ListType, error) {
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, 0, err
	}

	res, err := pc.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = res.Body.Close() }()

	return m3u8.DecodeFrom(res.Body, false)
}

func (pc *PlaylistClient) DecodeSegment(uri string, mode cipher.BlockMode, folder string, segmentNo int) error {
	res, err := pc.client.Get(uri)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}

	mode.CryptBlocks(body, body)

	lastByte := body[len(body)-1]
	lastByteInt := int(lastByte)

	if lastByteInt > 16 {
		return writeErrorSegmentFile(uri, folder, segmentNo, body)
	}

	padding := body[len(body)-int(lastByte):]

	dupes := make(map[byte]int, 0)
	for _, b := range padding {
		dupes[b] += 1
	}

	if len(dupes) != 1 || dupes[lastByte] != lastByteInt {
		return writeErrorSegmentFile(uri, folder, segmentNo, body)
	}

	if saveSegments {
		return writeSegmentFile(uri, folder, segmentNo, body)
	}

	return nil
}

func writeErrorSegmentFile(uri, folder string, segment int, body []byte) error {
	fmt.Printf("Error segment padding incorrect on segment: %s\n", uri)

	if err := os.MkdirAll(folder, os.ModePerm); err != nil {
		return err
	}

	file := fmt.Sprintf("%s/error_segment%d.m4f", folder, segment)
	out, err := os.Create(file)
	if err != nil {
		log.Fatal(err.Error())
	}

	defer func() {
		if err := out.Close(); err != nil {
			log.Fatal(err.Error())
		}
	}()

	if _, err = out.Write(body); err != nil {
		return err
	}

	return nil
}

func writeSegmentFile(uri, folder string, segment int, body []byte) error {
	if err := os.MkdirAll(folder, os.ModePerm); err != nil {
		return err
	}

	file := fmt.Sprintf("%s/segment%d.m4f", folder, segment)
	out, err := os.Create(file)
	if err != nil {
		log.Fatal(err.Error())
	}

	defer func() {
		if err := out.Close(); err != nil {
			log.Fatal(err.Error())
		}
	}()

	if _, err = out.Write(body); err != nil {
		return err
	}

	return nil
}

func newError(msg string) error {
	return errors.New("error: " + msg)
}

func (pc *PlaylistClient) GetCBCDecrypter(keyURI string, ivHEX string) (cipher.BlockMode, error) {
	res, err := pc.client.Get(keyURI)
	if err != nil {
		return nil, err
	}

	defer func() { _ = res.Body.Close() }()

	key, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	iv, err := hex.DecodeString(ivHEX[2:])
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	if len(iv) != aes.BlockSize {
		return nil, newError("IV length must be equal block size")
	}

	return cipher.NewCBCDecrypter(block, iv), nil
}
