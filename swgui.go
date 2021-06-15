package jsonrpc

// SwguiSettings adds JSON-RPC request interceptor to Swagger UI settings.
func SwguiSettings(settingsUI map[string]string, rpcPath string) map[string]string {
	if settingsUI == nil {
		settingsUI = make(map[string]string)
	}

	settingsUI["requestInterceptor"] = `function(request) {
				if (request.loadSpec) {
					return request;
				}
				var url = window.location.protocol + '//'+ window.location.host;
				var method = request.url.substring(url.length);
				request.url = url + '` + rpcPath + `';
				request.body = '{"jsonrpc": "2.0", "method": "' + method + '", "id": 1, "params": ' + request.body + '}';
				return request;
			}`

	return settingsUI
}
