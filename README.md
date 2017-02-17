# esperf

Is a multiplatform command line tool to load test and collect metrics from your [ElasticSearch](https://github.com/elastic/elasticsearch) deployment. It provides:

* Ability to query terms from a dictionary of words
* Send load following a constant or Poisson distribution
* Collect the following metrics:
     * Overall CPU load
     * GC time and count (broken by old and young collectors)
     * Latency 50, 90, 99, 99.9 percentiles
     * Memory pools usage (broken  by young, survivor and old)
     * Throughput

## Simple Usage

```sh
$ go get github.com/danielfireman/esperf
$ echo "word" > tiny_dict.txt
$ mkdir results
$ ./esperf --addr http://{ES_SERVER_IP}:9200 \
--load poisson:100 \
--duration 1m \
--cint 1s \
--results_path=$PWD/results \
--dict=$PWD/tiny_dict.txt
```

## Why esperf exists?

When researching for tools to load test ES I was quickly reminded by ES REST beauties and pointed out to [JMeter ](http://jmeter.apache.org/) or tools alike. As I needed to conduct experiments with very specific needs, I found that would be easier to build a tool myself than work around.

On the metrics side, I took a look at some ES plugins (i.e. [Marvel](https://www.elastic.co/downloads/marvel)) but I also bumped into some restrictions, for instance tune the metrics collection interval, pic and choose metrics and have access to raw data (CSV) to play with them in platforms like [R](https://www.r-project.org/) :heart:
