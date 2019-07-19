package requests

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"statusok/database"
	"statusok/notify"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
	"strings"
	"github.com/tidwall/gjson"
)

var (
	RequestsList   []RequestConfig
	requestChannel chan RequestConfig
	throttle       chan int
)

const (
	ContentType     = "Content-Type"
	ContentLength   = "Content-Length"
	FormContentType = "application/x-www-form-urlencoded"
	JsonContentType = "application/json"

	DefaultTime         = "300s"
	DefaultResponseCode = http.StatusOK
	DefaultConcurrency  = 1
)

type RequestConfig struct {
	Id           int
	Url          string            `json:"url"`
	RequestType  string            `json:"requestType"`
	Headers      map[string]string `json:"headers"`
	FormParams   map[string]string `json:"formParams"`
	UrlParams    map[string]string `json:"urlParams"`
	ResponseCode int               `json:"responseCode"`
	ResponseTime int64             `json:"responseTime"`
	ResponseBody []ResponseConfig  `json:"responseBody"`
	CheckEvery   time.Duration     `json:"checkEvery"`
}

type ResponseConfig struct {
	ResponseValue         string
	Valid                 bool
	Key                   string      `json:key`
	Empty                 bool        `json:isEmpty`
	CharCountGreaterThan  int64       `json:charCountGreaterThan`
	Int                   bool        `json:isInt`
	ErrorMessage          []string
}

//Set Id for request
func (requestConfig *RequestConfig) SetId(id int) {
	requestConfig.Id = id
}

//check whether all requestConfig fields are valid
func (requestConfig *RequestConfig) Validate() error {

	if len(requestConfig.Url) == 0 {
		return errors.New("Invalid Url")
	}

	if _, err := url.Parse(requestConfig.Url); err != nil {
		return errors.New("Invalid Url")
	}

	if len(requestConfig.RequestType) == 0 {
		return errors.New("RequestType cannot be empty")
	}

	if requestConfig.ResponseTime == 0 {
		return errors.New("ResponseTime cannot be empty")
	}

	if requestConfig.ResponseCode == 0 {
		requestConfig.ResponseCode = DefaultResponseCode
	}

	if requestConfig.CheckEvery == 0 {
		defTime, _ := time.ParseDuration(DefaultTime)
		requestConfig.CheckEvery = defTime
	}

	return nil
}

//Initialize data from config file and check all requests
func RequestsInit(data []RequestConfig, concurrency int) {
	RequestsList = data

	//throttle channel is used to limit number of requests performed at a time
	if concurrency == 0 {
		throttle = make(chan int, DefaultConcurrency)
	} else {
		throttle = make(chan int, concurrency)
	}

	requestChannel = make(chan RequestConfig, len(data))

	if len(data) == 0 {
		println("\nNo requests to monitor.Please add requests to you config file")
		os.Exit(3)
	}
	//send requests to make sure every every request is valid
	println("\nSending requests to apis.....making sure everything is right before we start monitoring")
	println("Api Count: ", len(data))

	for i, requestConfig := range data {
		println("Request #", i, " : ", requestConfig.RequestType, " ", requestConfig.Url)

		//Perform request
		reqErr := PerformRequest(requestConfig, nil)

		if reqErr != nil {
			//Request Failed
			println("\nFailed !!!! Not able to perfome below request")
			println("\n----Request Deatails---")
			println("Url :", requestConfig.Url)
			println("Type :", requestConfig.RequestType)
			println("Error Reason :", reqErr.Error())
			println("\nPlease check the config file and try again")
			os.Exit(3)
		}
	}

	println("All requests Successfull")
}

//Start monitoring by calling createTicker method for each request
func StartMonitoring() {
	fmt.Println("\nStarted Monitoring all ", len(RequestsList), " apis .....")

	go listenToRequestChannel()

	for _, requestConfig := range RequestsList {
		go createTicker(requestConfig)
	}
}

//A time ticker writes data to request channel for every request.CheckEvery seconds
func createTicker(requestConfig RequestConfig) {

	var ticker *time.Ticker = time.NewTicker(requestConfig.CheckEvery * time.Second)
	quit := make(chan struct{})
	for {
		select {
		case <-ticker.C:
			requestChannel <- requestConfig
		case <-quit:
			ticker.Stop()
			return
		}
	}
}

//all tickers write to request channel
//here we listen to request channel and perfom each request
func listenToRequestChannel() {

	//throttle is used to limit number of requests executed at a time
	for {
		select {
		case requect := <-requestChannel:
			throttle <- 1
			go PerformRequest(requect, throttle)
		}
	}

}

//takes the date from requestConfig and creates http request and executes it
func PerformRequest(requestConfig RequestConfig, throttle chan int) error {
	//Remove value from throttel channel when request is completed
	defer func() {
		if throttle != nil {
			<-throttle
		}
	}()

	var request *http.Request
	var reqErr error

	if len(requestConfig.FormParams) == 0 {
		//formParams create a request
		request, reqErr = http.NewRequest(requestConfig.RequestType,
			requestConfig.Url,
			nil)

	} else {
		if requestConfig.Headers[ContentType] == JsonContentType {
			//create a request using using formParams

			jsonBody, jsonErr := GetJsonParamsBody(requestConfig.FormParams)
			if jsonErr != nil {
				//Not able to create Request object.Add Error to Database
				go database.AddErrorInfo(database.ErrorInfo{
					Id:           requestConfig.Id,
					Url:          requestConfig.Url,
					RequestType:  requestConfig.RequestType,
					ResponseCode: 0,
					ResponseBody: "",
					Reason:       database.ErrCreateRequest,
					OtherInfo:    jsonErr.Error(),
				})

				return jsonErr
			}
			request, reqErr = http.NewRequest(requestConfig.RequestType,
				requestConfig.Url,
				jsonBody)

		} else {
			//create a request using formParams
			formParams := GetUrlValues(requestConfig.FormParams)

			request, reqErr = http.NewRequest(requestConfig.RequestType,
				requestConfig.Url,
				bytes.NewBufferString(formParams.Encode()))

			request.Header.Add(ContentLength, strconv.Itoa(len(formParams.Encode())))

			if requestConfig.Headers[ContentType] != "" {
				//Add content type to header if user doesnt mention it config file
				//Default content type application/x-www-form-urlencoded
				request.Header.Add(ContentType, FormContentType)
			}
		}
	}

	if reqErr != nil {
		//Not able to create Request object.Add Error to Database
		go database.AddErrorInfo(database.ErrorInfo{
			Id:           requestConfig.Id,
			Url:          requestConfig.Url,
			RequestType:  requestConfig.RequestType,
			ResponseCode: 0,
			ResponseBody: "",
			Reason:       database.ErrCreateRequest,
			OtherInfo:    reqErr.Error(),
		})

		return reqErr
	}

	//add url parameters to query if present
	if len(requestConfig.UrlParams) != 0 {
		urlParams := GetUrlValues(requestConfig.UrlParams)
		request.URL.RawQuery = urlParams.Encode()
	}

	//Add headers to the request
	AddHeaders(request, requestConfig.Headers)

	//TODO: put timeout ?
	/*
		timeout := 10 * requestConfig.ResponseTime

		client := &http.Client{
			Timeout: timeout,
		}
	*/

	client := &http.Client{}
	start := time.Now()

	getResponse, respErr := client.Do(request)

	respBody := convertResponseToString(getResponse)

	if respErr != nil {
		//Request failed . Add error info to database
		var statusCode int
		if getResponse == nil {
			statusCode = 0
		} else {
			statusCode = getResponse.StatusCode
		}
		go database.AddErrorInfo(database.ErrorInfo{
			Id:           requestConfig.Id,
			Url:          requestConfig.Url,
			RequestType:  requestConfig.RequestType,
			ResponseCode: statusCode,
			ResponseBody: respBody,
			Reason:       database.ErrDoRequest,
			OtherInfo:    respErr.Error(),
		})
		return respErr
	}

	defer getResponse.Body.Close()

	if getResponse.StatusCode != requestConfig.ResponseCode {
		//Response code is not the expected one .Add Error to database
		go database.AddErrorInfo(database.ErrorInfo{
			Id:           requestConfig.Id,
			Url:          requestConfig.Url,
			RequestType:  requestConfig.RequestType,
			ResponseCode: getResponse.StatusCode,
			ResponseBody: respBody,
			Reason:       errResposeCode(getResponse.StatusCode, requestConfig.ResponseCode),
			OtherInfo:    "",
		})
		return errResposeCode(getResponse.StatusCode, requestConfig.ResponseCode)
	}

	//Validate Response Body
	for body := range requestConfig.ResponseBody {
		respErr := ValidateResponse(respBody, requestConfig.ResponseBody[body])
		fmt.Println("Errors found")
		fmt.Println(respErr.ErrorMessage)
		if len(respErr.ErrorMessage) != 0 {
			var respMessage strings.Builder
			for i := range respErr.ErrorMessage {
				respMessage.WriteString("\n")
				respMessage.WriteString(respErr.ErrorMessage[i])
			}
			notify.SendErrorNotification(notify.ErrorNotification{requestConfig.Url,
				requestConfig.RequestType,
				respBody,
				"Response Body Validation",
				respMessage.String()})
		}
	}

	elapsed := time.Since(start)

	//Request succesfull . Add infomartion to Database
	go database.AddRequestInfo(database.RequestInfo{
		Id:                   requestConfig.Id,
		Url:                  requestConfig.Url,
		RequestType:          requestConfig.RequestType,
		ResponseCode:         getResponse.StatusCode,
		ResponseTime:         elapsed.Nanoseconds() / 1000000,
		ExpectedResponseTime: requestConfig.ResponseTime,
	})

	return nil
}

//convert response body to string
func convertResponseToString(resp *http.Response) string {
	if resp == nil {
		return " "
	}
	buf := new(bytes.Buffer)
	_, bufErr := buf.ReadFrom(resp.Body)

	if bufErr != nil {
		return " "
	}

	return buf.String()
}

//Add header values from map to request
func AddHeaders(req *http.Request, headers map[string]string) {
	for key, value := range headers {
		req.Header.Add(key, value)
	}
}

//convert params in map to url.Values
func GetUrlValues(params map[string]string) url.Values {
	urlParams := url.Values{}
	i := 0
	for key, value := range params {
		if i == 0 {
			urlParams.Set(key, value)
		} else {
			urlParams.Add(key, value)
		}
	}

	return urlParams
}

//Creates body for request of type application/json from map
func GetJsonParamsBody(params map[string]string) (io.Reader, error) {
	data, jsonErr := json.Marshal(params)

	if jsonErr != nil {

		jsonErr = errors.New("Invalid Parameters for Content-Type application/json : " + jsonErr.Error())

		return nil, jsonErr
	}

	return bytes.NewBuffer(data), nil
}

//creates an error when response code from server is not equal to response code mentioned in config file
func errResposeCode(status int, expectedStatus int) error {
	return errors.New(fmt.Sprintf("Got Response code %v. Expected Response Code %v ", status, expectedStatus))
}

func ValidateResponse(respBody string, responseConfig ResponseConfig) ResponseConfig {

	resp := gjson.Get(respBody, responseConfig.Key).String()
	responseConfig.ResponseValue = resp

	//Default Valid status to true
	responseConfig.Valid = true

	var errMessage []string

	//Validate whether the response is empty
	if responseConfig.Empty == true && resp != "" {
		responseConfig.Valid = false
		errMessage = append(errMessage, "Response is not empty")
	} else if responseConfig.Empty == false && resp == "" {
		responseConfig.Valid = false
		errMessage = append(errMessage, "Response is empty")
	}

	//Validate character count greater than
	if responseConfig.CharCountGreaterThan != 0 && int64(len(resp)) > responseConfig.CharCountGreaterThan {
		responseConfig.Valid = false
		errMessage = append(errMessage, "Character count is too small")
	}

	fmt.Println(responseConfig)
	if _, err := strconv.Atoi(resp); err != nil && responseConfig.Int == true {
		responseConfig.Valid = false
		errMessage = append(errMessage, "Response Value is not Integer")
	} else if _, err := strconv.Atoi(resp); err == nil && responseConfig.Int == false {
		responseConfig.Valid = false
		errMessage = append(errMessage, "Response Value is an Integer")
	}
	responseConfig.ErrorMessage = errMessage

	return responseConfig

}
