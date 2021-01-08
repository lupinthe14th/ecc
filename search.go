package main

import (
	"bytes"
	"context"
	b64 "encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"sort"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/urfave/cli/v2"
)

// AmplitudeID wraps the httpRequest.headers response.
type AmplitudeID struct {
	DeviceID       string `json:"deviceId"`
	UserID         string `json:"userId"`
	OptOut         bool   `json:"optOut"`
	SessionID      int64  `json:"sessionId"`
	LastEventTime  int64  `json:"lastEventTime"`
	EventID        int    `json:"eventId"`
	IdentifyID     int    `json:"identifyId"`
	SequenceNumber int    `json:"sequenceNumber"`
}

var searchCommand = &cli.Command{
	Name:   "search",
	Usage:  "Search elasticsearch",
	Action: searchAction,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "query",
			Value:   "",
			Aliases: []string{"q"},
			Usage:   "Specify query json file",
		},
		&cli.TimestampFlag{
			Name:     "since",
			Required: false,
			Layout:   "2006-01-02 15:04:05",
			Value:    cli.NewTimestamp(time.Now().Add(-30 * time.Minute)),
			Aliases:  []string{"S"},
			Usage:    "Start showing entries on or newer than the specified date respectively.",
		},
		&cli.TimestampFlag{
			Name:     "until",
			Required: false,
			Layout:   "2006-01-02 15:04:05",
			Value:    cli.NewTimestamp(time.Now()),
			Aliases:  []string{"U"},
			Usage:    "Start showing entries on or older than the specified date, respectively.",
		},
	},
}

func searchAction(c *cli.Context) error {
	// Search for the indexed documents
	//
	// Build the request body.
	w := c.App.Writer
	es, err := newClient(c)
	if err != nil {
		return err
	}

	filename := c.String("query")
	logrus.Debugf("filename: %s", filename)
	since := c.Timestamp("since").Format(time.RFC3339Nano)
	logrus.Debugf("since: %s", since)
	until := c.Timestamp("until").Format(time.RFC3339Nano)
	logrus.Debugf("until: %s", until)
	m, _ := time.ParseDuration("5m")
	query, err := buildQuery(filename, since, until)
	logrus.Debugf("query: %s", query)
	if err != nil {
		return err
	}
	// Perform the search request.
	res, err := es.Search(
		es.Search.WithContext(context.Background()),
		es.Search.WithIndex("log-aws-waf-*"),
		es.Search.WithBody(query),
		es.Search.WithPretty(),
		es.Search.WithSize(10000),
		es.Search.WithScroll(m),
		es.Search.WithSource("httpRequest.headers"),
		es.Search.WithSort("_doc:asc"),
	)
	if err != nil {
		return fmt.Errorf("Error getting response: %s", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		var e map[string]interface{}
		if err := json.NewDecoder(res.Body).Decode(&e); err != nil {
			return fmt.Errorf("Error parsing the response body: %s", err)
		}
		// Print the response status and error information.
		return fmt.Errorf("[%s] %s: %s",
			res.Status(),
			e["error"].(map[string]interface{})["type"],
			e["error"].(map[string]interface{})["reason"],
		)
	}

	var b bytes.Buffer
	b.ReadFrom(res.Body)
	total := gjson.GetBytes(b.Bytes(), "hits.total.value").Int()
	bar := pb.Start64(total)
	logrus.Infof("total hits: %v", total)
	hits := int64(len(gjson.GetBytes(b.Bytes(), "hits.hits").Array()))
	logrus.Debugf("hits: %v", hits)
	took := gjson.GetBytes(b.Bytes(), "took").Int()
	sid := gjson.GetBytes(b.Bytes(), "_scroll_id").String()
	logrus.Debugf("sid: %v", sid)

	amplitudeIDs := make([]AmplitudeID, 0, hits)

	for _, hit := range gjson.GetBytes(b.Bytes(), "hits.hits").Array() {
		bar.Increment()
		headers := gjson.Get(hit.Map()["_source"].String(), "httpRequest.headers").Array()
		for _, header := range headers {
			if header.Map()["name"].Str == "cookie" {
				cookie := header.Map()["value"].Str
				amplitudeID, err := cookieToAmplitudeID(cookie)
				if err != nil {
					return err
				}
				if amplitudeID != (AmplitudeID{}) {
					amplitudeIDs = append(amplitudeIDs, amplitudeID)
				}
			}
		}
	}

	if total > hits {
		for hits > 0 {
			res, err := es.Scroll(
				es.Scroll.WithScrollID(sid),
				es.Scroll.WithScroll(m),
			)
			if err != nil {
				return fmt.Errorf("Error getting response: %s", err)
			}

			if res.IsError() {
				var e map[string]interface{}
				if err := json.NewDecoder(res.Body).Decode(&e); err != nil {
					return fmt.Errorf("Error parsing the response body: %s", err)
				}
				// Print the response status and error information.
				return fmt.Errorf("[%s] %s: %s",
					res.Status(),
					e["error"].(map[string]interface{})["type"],
					e["error"].(map[string]interface{})["reason"],
				)
			}

			var b bytes.Buffer
			b.ReadFrom(res.Body)
			for _, hit := range gjson.GetBytes(b.Bytes(), "hits.hits").Array() {
				bar.Increment()
				headers := gjson.Get(hit.Map()["_source"].String(), "httpRequest.headers").Array()
				for _, header := range headers {
					if header.Map()["name"].Str == "cookie" {
						cookie := header.Map()["value"].Str
						amplitudeID, err := cookieToAmplitudeID(cookie)
						if err != nil {
							return err
						}
						if amplitudeID != (AmplitudeID{}) {
							amplitudeIDs = append(amplitudeIDs, amplitudeID)
						}
					}
				}
			}
			hits = int64(len(gjson.GetBytes(b.Bytes(), "hits.hits").Array()))
			took += gjson.GetBytes(b.Bytes(), "took").Int()
			logrus.Debugf("hits: %v", hits)
			logrus.Debugf("amplitude Id: %v", len(amplitudeIDs))
			// in any case, only the most recently received _scroll_id should be used.
			// See: https://www.elastic.co/guide/en/elasticsearch/reference/master/paginate-search-results.html#scroll-search-results
			sid = gjson.GetBytes(b.Bytes(), "_scroll_id").String()
			logrus.Debugf("sid: %v", sid)
		}
	}
	bar.Finish()
	out, err := json.Marshal(&amplitudeIDs)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%v\n", string(out))

	logrus.Infof("amplitude Id count: %v", len(amplitudeIDs))
	logrus.Infof(
		"[%s] %d hits; took: %dms\n",
		res.Status(),
		total,
		took,
	)
	printAmplitudeIDSummary(amplitudeIDs)
	return nil
}

func printAmplitudeIDSummary(amplitudeIDs []AmplitudeID) {
	memo := make(map[string]int)
	for _, v := range amplitudeIDs {
		memo[v.UserID]++
	}
	type user struct {
		uuid  string
		count int
	}
	userIDs := make([]user, 0, len(memo))
	for k, v := range memo {
		userIDs = append(userIDs, user{uuid: k, count: v})
	}
	sort.SliceStable(userIDs, func(i, j int) bool {
		if userIDs[i].count == userIDs[j].count {
			return userIDs[i].uuid < userIDs[j].uuid
		}
		return userIDs[i].count < userIDs[j].count
	})
	for i, userID := range userIDs {
		logrus.Infof("%v: %v: %v", i+1, userID.uuid, userID.count)
	}
}

// cookieToAmplitudeID is AmplitudeID extractiong from cookie
func cookieToAmplitudeID(cookie string) (AmplitudeID, error) {
	var amplitudeID AmplitudeID
	for _, values := range strings.Split(cookie, ";") {
		if strings.Contains(values, "amplitude_id") {
			sEnc := trimNextEqual(values)
			sDec, err := b64.StdEncoding.DecodeString(sEnc)
			if err != nil {
				return (AmplitudeID{}), fmt.Errorf("Error encodeings the amplitude_id value: %s", err)
			}
			err = json.Unmarshal(sDec, &amplitudeID)
			if err != nil {
				return (AmplitudeID{}), fmt.Errorf("Error to unmarshal JSON into AmplitudeID struct: %s", err)
			}
		}
	}
	return amplitudeID, nil
}

// trimNexEqual は最初の=の次から末尾までの文字列を返す
func trimNextEqual(s string) string {
	i := 0
	for i = 0; i < len(s); i++ {
		if s[i] == '=' {
			break
		}
	}
	return s[i+1:]
}

func buildQuery(filename, since, until string) (io.Reader, error) {
	if filename == "" {
		var b strings.Builder
		b.WriteString(fmt.Sprintf(query, since, until))
		return strings.NewReader(b.String()), nil
	}
	logrus.Debugf("filename: %v", filename)
	query, err := ioutil.ReadFile(filename)
	logrus.Debugf("query: %v", query)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(query), nil
}

const query = `{
  "query": {
    "bool": {
      "must": [
        {
          "match_all": {}
        }
      ],
      "filter": [
        {
          "bool": {
            "filter": [
              {
                "bool": {
                  "must_not": {
                    "bool": {
                      "should": [
                        {
                          "match": {
                            "ruleGroupList.terminatingRule.ruleId": "HostingProviderIPList"
                          }
                        }
                      ],
                      "minimum_should_match": 1
                    }
                  }
                }
              },
              {
                "bool": {
                  "filter": [
                    {
                      "bool": {
                        "should": [
                          {
                            "match": {
                              "ruleGroupList.terminatingRule.action": "BLOCK"
                            }
                          }
                        ],
                        "minimum_should_match": 1
                      }
                    },
                    {
                      "bool": {
                        "filter": [
                          {
                            "bool": {
                              "should": [
                                {
                                  "match": {
                                    "ruleGroupList.terminatingRule.ruleId": "AnonymousIPList"
                                  }
                                }
                              ],
                              "minimum_should_match": 1
                            }
                          },
                          {
                            "bool": {
                              "filter": [
                                {
                                  "bool": {
                                    "should": [
                                      {
                                        "range": {
                                          "httpRequest.clientIp": {
                                            "gte": "103.208.220.0"
                                          }
                                        }
                                      }
                                    ],
                                    "minimum_should_match": 1
                                  }
                                },
                                {
                                  "bool": {
                                    "should": [
                                      {
                                        "range": {
                                          "httpRequest.clientIp": {
                                            "lte": "103.208.223.255"
                                          }
                                        }
                                      }
                                    ],
                                    "minimum_should_match": 1
                                  }
                                }
                              ]
                            }
                          }
                        ]
                      }
                    }
                  ]
                }
              }
            ]
          }
        },
        {
          "match_phrase": {
            "rule.ruleset": "wafv2-linux"
          }
        },
        {
          "range": {
            "@timestamp": {
              "gte": %q,
              "lte": %q,
              "format": "strict_date_optional_time"
            }
          }
        }
      ]
    }
  }
}`
