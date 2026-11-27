import { describe, expect, it } from 'vitest';
import { historyPath, matchCluster, pickHub, proxyBase, uiLink } from './paths';

const ref = { cluster: 'prod', group: 'apps', kind: 'Deployment', namespace: 'shop', name: 'api' };

describe('mnemo', () => {
  it('reaches the hub through the service proxy', () => {
    expect(proxyBase('mnemo', 'mnemo')).toBe('/api/v1/namespaces/mnemo/services/mnemo:http/proxy');
  });

  it('picks the hub Service, not the metrics one', () => {
    const svc = (name: string, component?: string, port = 'http') => ({
      metadata: {
        name,
        namespace: 'mnemo',
        labels: component ? { 'app.kubernetes.io/component': component } : {},
      },
      spec: { ports: [{ name: port }] },
    });
    expect(pickHub([svc('mnemo-metrics', 'metrics'), svc('mnemo')])?.metadata.name).toBe('mnemo');
    expect(
      pickHub([svc('mnemo-metrics', 'metrics'), svc('other', undefined, 'grpc')])
    ).toBeUndefined();
  });

  it('matches the cluster by kube-system UID first, then by name', () => {
    const clusters = [
      { name: 'prod', uid: 'u1' },
      { name: 'kind-dev', uid: 'u2' },
    ];
    expect(matchCluster(clusters, 'u1', 'kind-dev')).toBe('prod');
    expect(matchCluster(clusters, undefined, 'kind-dev')).toBe('kind-dev');
    expect(matchCluster(clusters, 'u9', 'nope')).toBeUndefined();
    expect(matchCluster(clusters, 'u9', 'prod')).toBeUndefined(); // same name, another cluster
  });

  it('builds API paths and UI links', () => {
    expect(historyPath(ref)).toBe(
      '/api/v1/history?cluster=prod&group=apps&kind=Deployment&namespace=shop&name=api&limit=20'
    );
    expect(uiLink('javascript:alert(1)', ref)).toBeUndefined();
    expect(uiLink('https://mnemo.example.com/', ref)).toBe(
      'https://mnemo.example.com/?cluster=prod&group=apps&kind=Deployment&namespace=shop&name=api'
    );
  });
});
