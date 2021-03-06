/*
* Copyright 2015 Axibase Corporation or its affiliates. All Rights Reserved.
*
* Licensed under the Apache License, Version 2.0 (the "License").
* You may not use this file except in compliance with the License.
* A copy of the License is located at
*
* https://www.axibase.com/atsd/axibase-apache-2.0.pdf
*
* or in the "license" file accompanying this file. This file is distributed
* on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
* express or implied. See the License for the specific language governing
* permissions and limitations under the License.
 */

package storage

import (
	"sync/atomic"
	"time"

	"github.com/golang/glog"

	"github.com/axibase/atsd-api-go/http"
	"github.com/axibase/atsd-api-go/net"
)

type HttpCommunicator struct {
	client *http.Client

	seriesCommandsChunkChan chan *Chunk
	propertyCommands        chan []*net.PropertyCommand
	entityTag               chan []*net.EntityTagCommand
	messageCommands         chan []*net.MessageCommand
	counters                *httpCounters
}

type httpCounters struct {
	series, entityTag, prop, messages struct{ sent, dropped uint64 }
}

func NewHttpCommunicator(client *http.Client) *HttpCommunicator {
	hc := &HttpCommunicator{
		client:                  client,
		seriesCommandsChunkChan: make(chan *Chunk),
		propertyCommands:        make(chan []*net.PropertyCommand),
		entityTag:               make(chan []*net.EntityTagCommand),
		messageCommands:         make(chan []*net.MessageCommand),
		counters:                &httpCounters{},
	}
	go func() {
		for {
			expBackoff := NewExpBackoff(100*time.Millisecond, 5*time.Minute)
			select {
			case entityTag := <-hc.entityTag:
				entities := entityTagCommandsToEntities(entityTag)
				for _, entity := range entities {
					err := hc.client.Entities.Update(entity)
					if err != nil {
						tryWhileNotComplete(func() error { return hc.client.Entities.Create(entity) }, "entity update", expBackoff)
					}
					atomic.AddUint64(&hc.counters.entityTag.sent, 1)
				}

			case propertyCommands := <-hc.propertyCommands:
				if len(propertyCommands) > 0 {
					properties := propertyCommandsToProperties(propertyCommands)
					tryWhileNotComplete(func() error { return hc.client.Properties.Insert(properties) }, "properties insert", expBackoff)
					atomic.AddUint64(&hc.counters.prop.sent, uint64(len(properties)))
				}
			case messageCommands := <-hc.messageCommands:
				if len(messageCommands) > 0 {
					messages := messageCommandsToProperties(messageCommands)
					tryWhileNotComplete(func() error { return hc.client.Messages.Insert(messages) }, "messages insert", expBackoff)
					atomic.AddUint64(&hc.counters.messages.sent, uint64(len(messages)))
				}

			case seriesChunk := <-hc.seriesCommandsChunkChan:
				series := seriesCommandsChunkToSeries(seriesChunk)
				if len(series) > 0 {
					tryWhileNotComplete(func() error { return hc.client.Series.Insert(series) }, "series insert", expBackoff)
					atomic.AddUint64(&hc.counters.series.sent, uint64(len(series)))
				}
			}
			expBackoff.Reset()
		}
	}()

	return hc
}

func tryWhileNotComplete(task func() error, taskName string, expBackoff *ExpBackoff) {
	firstTime := true
	hasErrors := false
	for firstTime || hasErrors {
		firstTime = false
		err := task()
		hasErrors = err != nil
		if hasErrors {
			waitDuration := expBackoff.Duration()
			glog.Error("Could not perform ", taskName, ": ", err, "waiting for ", waitDuration)
			time.Sleep(waitDuration)
		} else {
			expBackoff.Reset()
		}
	}
}

func (self *HttpCommunicator) QueuedSendData(seriesCommandsChunk []*Chunk, entityTagCommands []*net.EntityTagCommand, propertyCommands []*net.PropertyCommand, messageCommands []*net.MessageCommand) {
	self.propertyCommands <- propertyCommands

	self.entityTag <- entityTagCommands

	self.messageCommands <- messageCommands

	for _, val := range seriesCommandsChunk {
		self.seriesCommandsChunkChan <- val
	}
}

func (self *HttpCommunicator) PriorSendData(seriesCommands []*net.SeriesCommand, entityTagCommands []*net.EntityTagCommand, propertyCommands []*net.PropertyCommand, messageCommands []*net.MessageCommand) {
	entities := entityTagCommandsToEntities(entityTagCommands)
	for _, entity := range entities {
		err := self.client.Entities.Update(entity)
		if err != nil {
			err = self.client.Entities.Create(entity)
			if err != nil {
				glog.Error("Could not prior send entity update: ", err)
			}
		}
	}
	if len(propertyCommands) > 0 {
		properties := propertyCommandsToProperties(propertyCommands)
		err := self.client.Properties.Insert(properties)
		if err != nil {
			glog.Error("Could not prior send property: ", err)
		}
	}

	if len(seriesCommands) > 0 {
		series := seriesCommandsToSeries(seriesCommands)
		err := self.client.Series.Insert(series)
		if err != nil {
			glog.Error("Could not prior send series: ", err)
		}
	}

	if len(messageCommands) > 0 {
		messages := messageCommandsToProperties(messageCommands)
		err := self.client.Messages.Insert(messages)
		if err != nil {
			glog.Error("Could not prior send message: ", err)
		}
	}
}
func (self *HttpCommunicator) SelfMetricValues() []*metricValue {
	return []*metricValue{
		{
			name: "series-commands.sent",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.series.sent)),
		},
		{
			name: "series-commands.dropped",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.series.dropped)),
		},
		{
			name: "message-commands.sent",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.messages.sent)),
		},
		{
			name: "message-commands.dropped",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.messages.dropped)),
		},
		{
			name: "property-commands.sent",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.prop.sent)),
		},
		{
			name: "property-commands.dropped",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.prop.dropped)),
		},
		{
			name: "entitytag-commands.sent",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.entityTag.sent)),
		},
		{
			name: "entitytag-commands.dropped",
			tags: map[string]string{
				"transport": self.client.Url().Scheme,
			},
			value: net.Int64(atomic.LoadUint64(&self.counters.entityTag.dropped)),
		},
	}
}

func seriesCommandsToSeries(seriesCommands []*net.SeriesCommand) []*http.Series {
	series := []*http.Series{}

	for _, command := range seriesCommands {
		metrics := command.Metrics()
		timestamp := command.Timestamp()
		if timestamp == nil {
			panic("Nil timestamp!")
		}
		tags := command.Tags()
		for key, val := range metrics {
			series = append(series,
				&http.Series{
					Entity: command.Entity(),
					Metric: key,
					Tags:   tags,
					Data: []*http.Sample{
						{
							T: *timestamp,
							V: val,
						},
					},
				})
		}
	}
	return series
}
func seriesCommandsChunkToSeries(seriesCommandsChunk *Chunk) []*http.Series {
	series := []*http.Series{}
	if seriesCommandsChunk.Len() > 0 {
		seriesMap := map[string]*http.Series{}
		for el := seriesCommandsChunk.Front(); el != nil; el = seriesCommandsChunk.Front() {
			seriesCommand := *el.Value.(*net.SeriesCommand)
			metrics := seriesCommand.Metrics()
			tags := seriesCommand.Tags()
			for key, val := range metrics {
				if _, ok := seriesMap[key]; !ok {
					seriesMap[key] = &http.Series{
						Entity: seriesCommand.Entity(),
						Metric: key,
						Tags:   tags,
					}
				}
				if seriesCommand.Timestamp() == nil {
					panic("Nil timestamp!")
				}
				seriesMap[key].Data = append(seriesMap[key].Data, &http.Sample{T: *seriesCommand.Timestamp(), V: val})
			}
			seriesCommandsChunk.Remove(el)
		}
		for _, s := range seriesMap {
			series = append(series, s)
		}
	}
	return series
}
func entityTagCommandsToEntities(entityTagCommands []*net.EntityTagCommand) []*http.Entity {
	entities := []*http.Entity{}

	for _, command := range entityTagCommands {
		entity := http.NewEntity(command.Entity())
		for key, value := range command.Tags() {
			entity.SetTag(key, value)
		}
		entities = append(entities, entity)
	}
	return entities
}
func propertyCommandsToProperties(propertyCommands []*net.PropertyCommand) []*http.Property {
	properties := []*http.Property{}
	for _, propertyCommand := range propertyCommands {
		property := http.NewProperty(propertyCommand.PropType(), propertyCommand.Entity()).
			SetKey(propertyCommand.Key()).
			SetAllTags(propertyCommand.Tags())
		if propertyCommand.Timestamp() != nil {
			property.SetTimestamp(*propertyCommand.Timestamp())
		}

		properties = append(properties, property)
	}
	return properties
}
func messageCommandsToProperties(messageCommands []*net.MessageCommand) []*http.Message {
	messages := []*http.Message{}
	for _, messageCommand := range messageCommands {
		message := http.NewMessage(messageCommand.Entity()).
			SetMessage(messageCommand.Message())
		for key, val := range messageCommand.Tags() {
			if key == "severity" {
				message.SetSeverity(http.Severity(val))
			}
			if key == "source" {
				message.SetSource(val)
			}
			if key == "type" {
				message.SetType(val)
			}
			message.SetTag(key, val)
		}
		if messageCommand.Timestamp() != nil {
			message.SetTimestamp(*messageCommand.Timestamp())
		}

		messages = append(messages, message)
	}
	return messages
}
