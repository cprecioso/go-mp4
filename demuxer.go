package mp4

import (
	"bytes"
	"fmt"
	"github.com/nareix/av"
	"github.com/nareix/mp4/atom"
	"github.com/nareix/mp4/isom"
	"github.com/nareix/codec/aacparser"
	"github.com/nareix/codec/h264parser"
	"io"
)

func Open(R io.ReadSeeker) (demuxer *Demuxer, err error) {
	_demuxer := &Demuxer{R: R}
	if err = _demuxer.ReadHeader(); err != nil {
		return
	}
	demuxer = _demuxer
	return
}

type Demuxer struct {
	R io.ReadSeeker

	streams   []*Stream
	movieAtom *atom.Movie
}

func (self *Demuxer) Streams() (streams []av.CodecData, err error) {
	if len(self.streams) == 0 {
		err = fmt.Errorf("mp4: no streams")
		return
	}
	for _, stream := range self.streams {
		streams = append(streams, stream)
	}
	return
}

func (self *Demuxer) ReadHeader() (err error) {
	var N int64
	var moov *atom.Movie

	if N, err = self.R.Seek(0, 2); err != nil {
		return
	}
	if _, err = self.R.Seek(0, 0); err != nil {
		return
	}

	lr := &io.LimitedReader{R: self.R, N: N}
	for lr.N > 0 {
		var ar *io.LimitedReader

		var cc4 string
		if ar, cc4, err = atom.ReadAtomHeader(lr, ""); err != nil {
			return
		}

		if cc4 == "moov" {
			if moov, err = atom.ReadMovie(ar); err != nil {
				return
			}
		}

		if _, err = atom.ReadDummy(lr, int(ar.N)); err != nil {
			return
		}
	}

	if moov == nil {
		err = fmt.Errorf("'moov' atom not found")
		return
	}
	self.movieAtom = moov

	self.streams = []*Stream{}
	for i, atrack := range moov.Tracks {
		stream := &Stream{
			trackAtom: atrack,
			r:         self.R,
			idx:       i,
		}
		if atrack.Media != nil && atrack.Media.Info != nil && atrack.Media.Info.Sample != nil {
			stream.sample = atrack.Media.Info.Sample
			stream.timeScale = int64(atrack.Media.Header.TimeScale)
		} else {
			err = fmt.Errorf("sample table not found")
			return
		}

		if avc1 := atom.GetAvc1ConfByTrack(atrack); avc1 != nil {
			if stream.CodecData, err = h264parser.NewCodecDataFromAVCDecoderConfRecord(avc1.Data); err != nil {
				return
			}
			self.streams = append(self.streams, stream)

		} else if mp4a := atom.GetMp4aDescByTrack(atrack); mp4a != nil && mp4a.Conf != nil {
			var config []byte
			if config, err = isom.ReadElemStreamDesc(bytes.NewReader(mp4a.Conf.Data)); err != nil {
				return
			}
			if stream.CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(config); err != nil {
				return
			}
			self.streams = append(self.streams, stream)

		}
	}

	return
}

func (self *Stream) setSampleIndex(index int) (err error) {
	found := false
	start := 0
	self.chunkGroupIndex = 0

	for self.chunkIndex = range self.sample.ChunkOffset.Entries {
		if self.chunkGroupIndex+1 < len(self.sample.SampleToChunk.Entries) &&
			self.chunkIndex+1 == self.sample.SampleToChunk.Entries[self.chunkGroupIndex+1].FirstChunk {
			self.chunkGroupIndex++
		}
		n := self.sample.SampleToChunk.Entries[self.chunkGroupIndex].SamplesPerChunk
		if index >= start && index < start+n {
			found = true
			self.sampleIndexInChunk = index - start
			break
		}
		start += n
	}
	if !found {
		err = fmt.Errorf("stream[%d]: cannot locate sample index in chunk", self.idx)
		return
	}

	if self.sample.SampleSize.SampleSize != 0 {
		self.sampleOffsetInChunk = int64(self.sampleIndexInChunk * self.sample.SampleSize.SampleSize)
	} else {
		if index >= len(self.sample.SampleSize.Entries) {
			err = fmt.Errorf("stream[%d]: sample index out of range", self.idx)
			return
		}
		self.sampleOffsetInChunk = int64(0)
		for i := index - self.sampleIndexInChunk; i < index; i++ {
			self.sampleOffsetInChunk += int64(self.sample.SampleSize.Entries[i])
		}
	}

	self.dts = int64(0)
	start = 0
	found = false
	self.sttsEntryIndex = 0
	for self.sttsEntryIndex < len(self.sample.TimeToSample.Entries) {
		entry := self.sample.TimeToSample.Entries[self.sttsEntryIndex]
		n := entry.Count
		if index >= start && index < start+n {
			self.sampleIndexInSttsEntry = index - start
			self.dts += int64((index - start) * entry.Duration)
			found = true
			break
		}
		start += n
		self.dts += int64(n * entry.Duration)
		self.sttsEntryIndex++
	}
	if !found {
		err = fmt.Errorf("stream[%d]: cannot locate sample index in stts entry", self.idx)
		return
	}

	if self.sample.CompositionOffset != nil && len(self.sample.CompositionOffset.Entries) > 0 {
		start = 0
		found = false
		self.cttsEntryIndex = 0
		for self.cttsEntryIndex < len(self.sample.CompositionOffset.Entries) {
			n := self.sample.CompositionOffset.Entries[self.cttsEntryIndex].Count
			if index >= start && index < start+n {
				self.sampleIndexInCttsEntry = index - start
				found = true
				break
			}
			start += n
			self.cttsEntryIndex++
		}
		if !found {
			err = fmt.Errorf("stream[%d]: cannot locate sample index in ctts entry", self.idx)
			return
		}
	}

	if self.sample.SyncSample != nil {
		self.syncSampleIndex = 0
		for self.syncSampleIndex < len(self.sample.SyncSample.Entries)-1 {
			if self.sample.SyncSample.Entries[self.syncSampleIndex+1]-1 > index {
				break
			}
			self.syncSampleIndex++
		}
	}

	if false {
		fmt.Printf("stream[%d]: setSampleIndex chunkGroupIndex=%d chunkIndex=%d sampleOffsetInChunk=%d\n",
			self.idx, self.chunkGroupIndex, self.chunkIndex, self.sampleOffsetInChunk)
	}

	self.sampleIndex = index
	return
}

func (self *Stream) isSampleValid() bool {
	if self.chunkIndex >= len(self.sample.ChunkOffset.Entries) {
		return false
	}
	if self.chunkGroupIndex >= len(self.sample.SampleToChunk.Entries) {
		return false
	}
	if self.sttsEntryIndex >= len(self.sample.TimeToSample.Entries) {
		return false
	}
	if self.sample.CompositionOffset != nil && len(self.sample.CompositionOffset.Entries) > 0 {
		if self.cttsEntryIndex >= len(self.sample.CompositionOffset.Entries) {
			return false
		}
	}
	if self.sample.SyncSample != nil {
		if self.syncSampleIndex >= len(self.sample.SyncSample.Entries) {
			return false
		}
	}
	if self.sample.SampleSize.SampleSize != 0 {
		if self.sampleIndex >= len(self.sample.SampleSize.Entries) {
			return false
		}
	}
	return true
}

func (self *Stream) incSampleIndex() (duration int64) {
	if false {
		fmt.Printf("incSampleIndex sampleIndex=%d sampleOffsetInChunk=%d sampleIndexInChunk=%d chunkGroupIndex=%d chunkIndex=%d\n",
			self.sampleIndex, self.sampleOffsetInChunk, self.sampleIndexInChunk, self.chunkGroupIndex, self.chunkIndex)
	}

	self.sampleIndexInChunk++
	if self.sampleIndexInChunk == self.sample.SampleToChunk.Entries[self.chunkGroupIndex].SamplesPerChunk {
		self.chunkIndex++
		self.sampleIndexInChunk = 0
		self.sampleOffsetInChunk = int64(0)
	} else {
		if self.sample.SampleSize.SampleSize != 0 {
			self.sampleOffsetInChunk += int64(self.sample.SampleSize.SampleSize)
		} else {
			self.sampleOffsetInChunk += int64(self.sample.SampleSize.Entries[self.sampleIndex])
		}
	}

	if self.chunkGroupIndex+1 < len(self.sample.SampleToChunk.Entries) &&
		self.chunkIndex+1 == self.sample.SampleToChunk.Entries[self.chunkGroupIndex+1].FirstChunk {
		self.chunkGroupIndex++
	}

	sttsEntry := self.sample.TimeToSample.Entries[self.sttsEntryIndex]
	duration = int64(sttsEntry.Duration)
	self.sampleIndexInSttsEntry++
	self.dts += duration
	if self.sampleIndexInSttsEntry == sttsEntry.Count {
		self.sampleIndexInSttsEntry = 0
		self.sttsEntryIndex++
	}

	if self.sample.CompositionOffset != nil && len(self.sample.CompositionOffset.Entries) > 0 {
		self.sampleIndexInCttsEntry++
		if self.sampleIndexInCttsEntry == self.sample.CompositionOffset.Entries[self.cttsEntryIndex].Count {
			self.sampleIndexInCttsEntry = 0
			self.cttsEntryIndex++
		}
	}

	if self.sample.SyncSample != nil {
		entries := self.sample.SyncSample.Entries
		if self.syncSampleIndex+1 < len(entries) && entries[self.syncSampleIndex+1]-1 == self.sampleIndex+1 {
			self.syncSampleIndex++
		}
	}

	self.sampleIndex++
	return
}

func (self *Stream) sampleCount() int {
	if self.sample.SampleSize.SampleSize == 0 {
		chunkGroupIndex := 0
		count := 0
		for chunkIndex := range self.sample.ChunkOffset.Entries {
			n := self.sample.SampleToChunk.Entries[chunkGroupIndex].SamplesPerChunk
			count += n
			if chunkGroupIndex+1 < len(self.sample.SampleToChunk.Entries) &&
				chunkIndex+1 == self.sample.SampleToChunk.Entries[chunkGroupIndex+1].FirstChunk {
				chunkGroupIndex++
			}
		}
		return count
	} else {
		return len(self.sample.SampleSize.Entries)
	}
}

func (self *Demuxer) ReadPacket() (streamIndex int, pkt av.Packet, err error) {
	var chosen *Stream
	for i, stream := range self.streams {
		if chosen == nil || stream.tsToTime(stream.dts) < chosen.tsToTime(chosen.dts) {
			chosen = stream
			streamIndex = i
		}
	}
	if false {
		fmt.Printf("ReadPacket: chosen index=%v time=%v\n", chosen.idx, chosen.tsToTime(chosen.dts))
	}
	pkt, err = chosen.readPacket()
	return
}

func (self *Demuxer) CurrentTime() (time float64) {
	if len(self.streams) > 0 {
		stream := self.streams[0]
		time = stream.tsToTime(stream.dts)
	}
	return
}

func (self *Demuxer) SeekToTime(time float64) (err error) {
	for _, stream := range self.streams {
		if stream.IsVideo() {
			if err = stream.seekToTime(time); err != nil {
				return
			}
			time = stream.tsToTime(stream.dts)
			break
		}
	}

	for _, stream := range self.streams {
		if !stream.IsVideo() {
			if err = stream.seekToTime(time); err != nil {
				return
			}
		}
	}

	return
}

func (self *Stream) readPacket() (pkt av.Packet, err error) {
	if !self.isSampleValid() {
		err = io.EOF
		return
	}
	//fmt.Println("readPacket", self.sampleIndex)

	chunkOffset := self.sample.ChunkOffset.Entries[self.chunkIndex]
	sampleSize := 0
	if self.sample.SampleSize.SampleSize != 0 {
		sampleSize = self.sample.SampleSize.SampleSize
	} else {
		sampleSize = self.sample.SampleSize.Entries[self.sampleIndex]
	}

	sampleOffset := int64(chunkOffset) + self.sampleOffsetInChunk
	if _, err = self.r.Seek(sampleOffset, 0); err != nil {
		return
	}

	pkt.Data = make([]byte, sampleSize)
	if _, err = self.r.Read(pkt.Data); err != nil {
		return
	}

	if self.sample.SyncSample != nil {
		if self.sample.SyncSample.Entries[self.syncSampleIndex]-1 == self.sampleIndex {
			pkt.IsKeyFrame = true
		}
	}

	//println("pts/dts", self.ptsEntryIndex, self.dtsEntryIndex)
	if self.sample.CompositionOffset != nil && len(self.sample.CompositionOffset.Entries) > 0 {
		cts := int64(self.sample.CompositionOffset.Entries[self.cttsEntryIndex].Offset)
		pkt.CompositionTime = self.tsToTime(cts)
	}

	duration := self.incSampleIndex()
	pkt.Duration = self.tsToTime(duration)

	return
}

func (self *Stream) seekToTime(time float64) (err error) {
	index := self.timeToSampleIndex(time)
	if err = self.setSampleIndex(index); err != nil {
		return
	}
	if false {
		fmt.Printf("stream[%d]: seekToTime index=%v time=%v cur=%v\n", self.idx, index, time, self.tsToTime(self.dts))
	}
	return
}

func (self *Stream) timeToSampleIndex(time float64) int {
	targetTs := self.timeToTs(time)
	targetIndex := 0

	startTs := int64(0)
	endTs := int64(0)
	startIndex := 0
	endIndex := 0
	found := false
	for _, entry := range self.sample.TimeToSample.Entries {
		endTs = startTs + int64(entry.Count*entry.Duration)
		endIndex = startIndex + entry.Count
		if targetTs >= startTs && targetTs < endTs {
			targetIndex = startIndex + int((targetTs-startTs)/int64(entry.Duration))
			found = true
		}
		startTs = endTs
		startIndex = endIndex
	}
	if !found {
		if targetTs < 0 {
			targetIndex = 0
		} else {
			targetIndex = endIndex - 1
		}
	}

	if self.sample.SyncSample != nil {
		entries := self.sample.SyncSample.Entries
		for i := len(entries) - 1; i >= 0; i-- {
			if entries[i]-1 < targetIndex {
				targetIndex = entries[i] - 1
				break
			}
		}
	}

	return targetIndex
}
