import {Redirect} from '@docusaurus/router';

export default function Home(): JSX.Element {
  return <Redirect to="/qemu-manage/docs/intro" />;
}
