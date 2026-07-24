import http from 'k6/http';
import { check } from 'k6';

const baseURL = (__ENV.BASE_URL || 'http://127.0.0.1:8080').replace(/\/$/, '');
const targetRPS = Number(__ENV.TARGET_RPS || 200);
const maxVUs = Number(__ENV.MAX_VUS || 100);

export const options = {
  scenarios: {
    health_baseline: {
      executor: 'constant-arrival-rate',
      rate: targetRPS,
      timeUnit: '1s',
      duration: __ENV.DURATION || '30s',
      preAllocatedVUs: 25,
      maxVUs,
    },
  },
  thresholds: {
    checks: ['rate>0.99'],
    dropped_iterations: ['count==0'],
    http_req_duration: ['p(95)<100', 'p(99)<250'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function () {
  const response = http.get(`${baseURL}/health`, {
    tags: { endpoint: 'health' },
  });

  check(response, {
    'health returns 200': (res) => res.status === 200,
    'health body is ok': (res) => res.json('status') === 'ok',
  });
}
