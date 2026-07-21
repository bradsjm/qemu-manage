import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  docs: [
    'intro',
    'getting-started',
    {
      type: 'category',
      label: 'Guides',
      items: [
        'guides/creating-vms',
        'guides/networking',
        'guides/starting-and-inspecting',
        'guides/monitor-and-guest-agent',
        'guides/log-management',
      ],
    },
    {
      type: 'category',
      label: 'Examples',
      items: [
        'examples/home-assistant',
        'examples/prometheus',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      items: [
        'operations/autostart',
        'operations/release-process',
      ],
    },
    {
      type: 'category',
      label: 'Reference',
      items: [
        'reference/cli',
        'reference/monitoring-api',
        'reference/configuration',
      ],
    },
    {
      type: 'category',
      label: 'Architecture',
      items: [
        'architecture/overview',
        'architecture/data-flow',
        'architecture/security',
      ],
    },
    'troubleshooting',
    'contributing',
  ],
};

export default sidebars;
