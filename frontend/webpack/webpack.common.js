// Общая часть конфигурации: точка входа, TypeScript, алиасы, HTML.
const path = require('path');
const HtmlWebpackPlugin = require('html-webpack-plugin');

const root = path.resolve(__dirname, '..');

module.exports = {
  entry: path.join(root, 'src/index.tsx'),
  output: {
    path: path.join(root, 'dist'),
    publicPath: '/',
    clean: true,
  },
  resolve: {
    extensions: ['.tsx', '.ts', '.js'],
    alias: { '@': path.join(root, 'src') },
  },
  module: {
    rules: [
      {
        test: /\.tsx?$/,
        exclude: /node_modules/,
        use: {
          loader: 'ts-loader',
          // Проверку типов делает отдельный `npm run typecheck` (tsc --noEmit),
          // сборка только транспилирует — она в разы быстрее.
          options: { transpileOnly: true },
        },
      },
      {
        test: /\.svg$/,
        type: 'asset',
        parser: { dataUrlCondition: { maxSize: 4 * 1024 } },
      },
      {
        test: /\.(png|jpe?g|webp|woff2?)$/,
        type: 'asset/resource',
      },
    ],
  },
  plugins: [
    new HtmlWebpackPlugin({
      template: path.join(root, 'public/index.html'),
      favicon: path.join(root, 'public/favicon.svg'),
    }),
  ],
};
