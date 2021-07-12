package influxdb

import (
	"context"
	"os"
	"time"

	ginmetrics "github.com/devopsfaith/krakend-metrics/gin"
	"github.com/influxdata/influxdb/client/v2"
	"github.com/luraproject/lura/config"
	"github.com/luraproject/lura/logging"

	"github.com/devopsfaith/krakend-influx/counter"
	"github.com/devopsfaith/krakend-influx/gauge"
	"github.com/devopsfaith/krakend-influx/histogram"
)

const Namespace = "github_com/letgoapp/krakend-influx"

type clientWrapper struct {
	influxClient client.Client
	collector    *ginmetrics.Metrics
	logger       logging.Logger
	db           string
	buf          *Buffer
	tags         map[string]string
}

func New(ctx context.Context, extraConfig config.ExtraConfig, metricsCollector *ginmetrics.Metrics, logger logging.Logger) error {
	logger.Debug("creating a new influxdb client")
	cfg, ok := configGetter(extraConfig).(influxConfig)
	if !ok {
		logger.Debug("no config for the influxdb client. Aborting")
		return errNoConfig
	}

	influxdbClient, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     cfg.address,
		Username: cfg.username,
		Password: cfg.password,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		logger.Debug("influxdb client crashed")
		return err
	}

	go func() {
		pingDuration, pingMsg, err := influxdbClient.Ping(time.Second)
		if err != nil {
			logger.Error("unable to ping the influx server:", err.Error())
			return
		}
		logger.Debug("influx ping results: duration =", pingDuration, "msg =", pingMsg)
	}()

	t := time.NewTicker(cfg.ttl)

	hostname, err := os.Hostname()
	if err != nil {
		logger.Error("influxdb resolving the local hostname:", err.Error())
	}
	cfg.tags["hostname"] = hostname

	cw := clientWrapper{
		influxClient: influxdbClient,
		collector:    metricsCollector,
		logger:       logger,
		db:           cfg.database,
		buf:          NewBuffer(cfg.bufferSize),
		tags:         cfg.tags,
	}

	go cw.keepUpdated(ctx, t.C)

	logger.Debug("influxdb client up and running")

	return nil
}

func (cw clientWrapper) keepUpdated(ctx context.Context, ticker <-chan time.Time) {
	for {
		select {
		case <-ticker:
		case <-ctx.Done():
			return
		}

		cw.logger.Debug("Preparing influxdb points")

		snapshot := cw.collector.Snapshot()

		if shouldSendPoints := len(snapshot.Counters) > 0 || len(snapshot.Gauges) > 0; !shouldSendPoints {
			cw.logger.Debug("no metrics to send to influx")
			continue
		}

		bp, _ := client.NewBatchPoints(client.BatchPointsConfig{
			Database:  cw.db,
			Precision: "s",
		})
		now := time.Unix(0, snapshot.Time)

		for _, p := range counter.Points(cw.tags, now, snapshot.Counters, cw.logger) {
			bp.AddPoint(p)
		}

		for _, p := range gauge.Points(cw.tags, now, snapshot.Gauges, cw.logger) {
			bp.AddPoint(p)
		}

		for _, p := range histogram.Points(cw.tags, now, snapshot.Histograms, cw.logger) {
			bp.AddPoint(p)
		}

		if err := cw.influxClient.Write(bp); err != nil {
			cw.logger.Error("writing to influx:", err.Error())
			cw.buf.Add(bp)
			continue
		}

		cw.logger.Info(len(bp.Points()), "datapoints sent to Influx")

		pts := []*client.Point{}
		bpPending := cw.buf.Elements()
		for _, failedBP := range bpPending {
			pts = append(pts, failedBP.Points()...)
		}

		retryBatch, _ := client.NewBatchPoints(client.BatchPointsConfig{
			Database:  cw.db,
			Precision: "s",
		})
		retryBatch.AddPoints(pts)

		if err := cw.influxClient.Write(retryBatch); err != nil {
			cw.logger.Error("writting to influx:", err.Error())
			cw.buf.Add(bpPending...)
			continue
		}
	}
}
