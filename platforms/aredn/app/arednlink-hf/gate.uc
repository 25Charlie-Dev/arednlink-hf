// SPDX-License-Identifier: GPL-3.0-only
export function preflight(env) {
    if (env.REQUEST_URI !== '/cgi-bin/apps/arednlink-hf/admin')
        return '404 Not Found';
    if (env.REQUEST_METHOD !== 'GET' && env.REQUEST_METHOD !== 'POST')
        return '405 Method Not Allowed';
    if (env.QUERY_STRING || env.headers?.['transfer-encoding'])
        return '400 Bad Request';
    const size = env.CONTENT_LENGTH ?? '0';
    if (!match(size, /^(0|[1-9][0-9]{0,5})$/) || int(size) > 65536)
        return '413 Payload Too Large';
    if (env.REQUEST_METHOD === 'GET' && int(size) !== 0)
        return '400 Bad Request';
    if (env.REQUEST_METHOD === 'POST' && env.CONTENT_TYPE !== 'text/plain;charset=UTF-8')
        return '415 Unsupported Media Type';
    return null;
};

export function csrf(env) {
    const headers = env.headers ?? {};
    const host = env.HTTP_HOST;
    if (!host || !(match(host, /^[-A-Za-z0-9.:]+$/) ||
        match(host, /^\[[0-9A-Fa-f:]+\](:[0-9]+)?$/)))
        return false;
    const scheme = env.HTTPS === 'on' ? 'https' : 'http';
    if (headers.origin !== `${scheme}://${host}` ||
        (headers['sec-fetch-site'] && headers['sec-fetch-site'] !== 'same-origin'))
        return false;
    const token = headers['x-arednlink-csrf'];
    if (!token || !match(token, /^[0-9a-f]{64}$/))
        return false;
    const cookies = split(headers.cookie ?? '', ';');
    let found = 0;
    let valid = false;
    for (let i = 0; i < length(cookies); i++) {
        const cookie = trim(cookies[i]);
        if (index(cookie, 'arednlinkCSRF=') === 0) {
            found++;
            valid = cookie === `arednlinkCSRF=${token}`;
        }
    }
    return found === 1 && valid;
};
